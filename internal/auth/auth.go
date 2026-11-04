package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"flow/internal/model"
	"flow/internal/store"
)

var (
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrUnauthenticated    = errors.New("authentication required")
	ErrForbidden          = errors.New("permission denied")
	ErrInvalid            = errors.New("invalid request")
)

const (
	KindSession = "session"
	KindAPI     = "api"

	SessionTTL     = 7 * 24 * time.Hour
	sessionPrefix  = "fs_"
	apiTokenPrefix = "fp_"
	minPassword    = 8
)

var usernameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{1,31}$`)
var tenantIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,31}$`)

// Principal 是已认证的请求主体。
type Principal struct {
	User     *model.User
	TenantID string
	Role     model.Role
	Token    *model.Token
}

// Can 判断主体在当前租户是否具备至少 role 的权限。
func (p *Principal) Can(role model.Role) bool { return p.Role.Rank() >= role.Rank() }

type Service struct {
	store store.Store
	now   func() time.Time
}

func NewService(s store.Store) *Service { return &Service{store: s, now: time.Now} }

// Bootstrap 在没有任何用户时创建系统管理员 admin（默认租户管理员）。password 为空时随机生成并返回。
func (s *Service) Bootstrap(ctx context.Context, password string) (created bool, generated string, err error) {
	n, err := s.store.CountUsers(ctx)
	if err != nil || n > 0 {
		return false, "", err
	}
	if password == "" {
		b := make([]byte, 9)
		rand.Read(b)
		password = hex.EncodeToString(b)
		generated = password
	}
	u, err := s.CreateUser(ctx, "admin", "管理员", password, true)
	if err != nil {
		return false, "", err
	}
	if err := s.store.PutMembership(ctx, &model.Membership{TenantID: "default", UserID: u.ID, Role: model.RoleAdmin}); err != nil {
		return false, "", err
	}
	return true, generated, nil
}

// SessionMeta 是登录时的设备信息，显示在「安全 → 活动会话」中。
type SessionMeta struct {
	UserAgent string
	IP        string
}

// Login 校验密码并创建会话令牌，当前租户为用户加入的第一个租户。
func (s *Service) Login(ctx context.Context, username, password string) (string, *Principal, error) {
	return s.LoginFrom(ctx, username, password, SessionMeta{})
}

// LoginFrom 同 Login，并记录会话的设备信息。
func (s *Service) LoginFrom(ctx context.Context, username, password string, meta SessionMeta) (string, *Principal, error) {
	u, err := s.store.GetUserByName(ctx, strings.TrimSpace(username))
	if errors.Is(err, store.ErrNotFound) {
		CheckPassword(dummyHash, password) // 平衡耗时，避免枚举用户名
		return "", nil, ErrInvalidCredentials
	}
	if err != nil {
		return "", nil, err
	}
	if !CheckPassword(u.PasswordHash, password) {
		return "", nil, ErrInvalidCredentials
	}
	tenant, err := s.defaultTenant(ctx, u)
	if err != nil {
		return "", nil, err
	}
	plain, tok, err := s.issue(ctx, u.ID, tenant, KindSession, "", SessionTTL, func(t *model.Token) {
		t.UserAgent, t.IP = truncate(meta.UserAgent, 300), truncate(meta.IP, 64)
	})
	if err != nil {
		return "", nil, err
	}
	p, err := s.principal(ctx, u, tok)
	return plain, p, err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

var dummyHash = HashPassword("dummy-password-for-timing")

func (s *Service) defaultTenant(ctx context.Context, u *model.User) (string, error) {
	ms, err := s.store.UserMemberships(ctx, u.ID)
	if err != nil {
		return "", err
	}
	for _, m := range ms {
		if m.TenantID == "default" {
			return m.TenantID, nil
		}
	}
	if len(ms) > 0 {
		return ms[0].TenantID, nil
	}
	if u.IsAdmin {
		return "default", nil
	}
	return "", fmt.Errorf("%w: user %s does not belong to any tenant", ErrForbidden, u.Username)
}

func (s *Service) issue(ctx context.Context, userID, tenantID, kind, name string, ttl time.Duration, opts ...func(*model.Token)) (string, *model.Token, error) {
	prefix := sessionPrefix
	if kind == KindAPI {
		prefix = apiTokenPrefix
	}
	plain, hash, display := NewToken(prefix)
	now := s.now()
	t := &model.Token{ID: randID(), Hash: hash, UserID: userID, TenantID: tenantID, Kind: kind, Name: name, Prefix: display, CreatedAt: now}
	if ttl > 0 {
		exp := now.Add(ttl)
		t.ExpiresAt = &exp
	}
	for _, o := range opts {
		o(t)
	}
	if err := s.store.CreateToken(ctx, t); err != nil {
		return "", nil, err
	}
	return plain, t, nil
}

// Authenticate 解析会话或 API 令牌。
func (s *Service) Authenticate(ctx context.Context, plain string) (*Principal, error) {
	if plain == "" {
		return nil, ErrUnauthenticated
	}
	tok, err := s.store.GetTokenByHash(ctx, HashToken(plain))
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrUnauthenticated
	}
	if err != nil {
		return nil, err
	}
	now := s.now()
	if tok.ExpiresAt != nil && !tok.ExpiresAt.After(now) {
		return nil, ErrUnauthenticated
	}
	u, err := s.store.GetUser(ctx, tok.UserID)
	if err != nil {
		return nil, ErrUnauthenticated
	}
	// 每分钟最多记录一次使用时间，减少写入。
	if tok.LastUsedAt == nil || now.Sub(*tok.LastUsedAt) > time.Minute {
		_ = s.store.TouchToken(ctx, tok.ID, now)
	}
	return s.principal(ctx, u, tok)
}

func (s *Service) principal(ctx context.Context, u *model.User, tok *model.Token) (*Principal, error) {
	p := &Principal{User: u, TenantID: tok.TenantID, Token: tok}
	ms, err := s.store.UserMemberships(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	for _, m := range ms {
		if m.TenantID == tok.TenantID {
			p.Role = m.Role
		}
	}
	if u.IsAdmin {
		p.Role = model.RoleAdmin // 系统管理员在所有租户中都是管理员
	}
	if p.Role == "" {
		return nil, fmt.Errorf("%w: no access to tenant %s", ErrForbidden, tok.TenantID)
	}
	return p, nil
}

func (s *Service) Logout(ctx context.Context, p *Principal) error {
	return s.store.DeleteToken(ctx, p.Token.ID)
}

// SwitchTenant 切换会话的当前租户（API 令牌固定在创建时的租户）。
func (s *Service) SwitchTenant(ctx context.Context, p *Principal, tenantID string) error {
	if p.Token.Kind != KindSession {
		return fmt.Errorf("%w: API tokens are bound to one tenant", ErrInvalid)
	}
	if _, err := s.store.GetTenant(ctx, tenantID); err != nil {
		return err
	}
	if !p.User.IsAdmin {
		ok := false
		ms, err := s.store.UserMemberships(ctx, p.User.ID)
		if err != nil {
			return err
		}
		for _, m := range ms {
			ok = ok || m.TenantID == tenantID
		}
		if !ok {
			return ErrForbidden
		}
	}
	return s.store.SetTokenTenant(ctx, p.Token.ID, tenantID)
}

// TenantView 是用户可访问的租户及其角色。
type TenantView struct {
	model.Tenant
	Role model.Role `json:"role"`
}

func (s *Service) Tenants(ctx context.Context, u *model.User) ([]TenantView, error) {
	all, err := s.store.ListTenants(ctx)
	if err != nil {
		return nil, err
	}
	ms, err := s.store.UserMemberships(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	roles := map[string]model.Role{}
	for _, m := range ms {
		roles[m.TenantID] = m.Role
	}
	var out []TenantView
	for _, t := range all {
		role, ok := roles[t.ID]
		if u.IsAdmin {
			role, ok = model.RoleAdmin, true
		}
		if ok {
			out = append(out, TenantView{Tenant: *t, Role: role})
		}
	}
	return out, nil
}

// CreateTenant 仅系统管理员可用，创建者自动成为该租户管理员。
func (s *Service) CreateTenant(ctx context.Context, p *Principal, id, name string) (*model.Tenant, error) {
	if !p.User.IsAdmin {
		return nil, ErrForbidden
	}
	if !tenantIDRE.MatchString(id) {
		return nil, fmt.Errorf("%w: tenant id must be 2-32 lowercase letters, digits or -", ErrInvalid)
	}
	if strings.TrimSpace(name) == "" {
		name = id
	}
	t := &model.Tenant{ID: id, Name: strings.TrimSpace(name), CreatedAt: s.now()}
	if err := s.store.CreateTenant(ctx, t); err != nil {
		return nil, err
	}
	return t, s.store.PutMembership(ctx, &model.Membership{TenantID: id, UserID: p.User.ID, Role: model.RoleAdmin})
}

func (s *Service) CreateUser(ctx context.Context, username, display, password string, isAdmin bool) (*model.User, error) {
	if !usernameRE.MatchString(username) {
		return nil, fmt.Errorf("%w: username must be 2-32 letters, digits, '.', '_' or '-'", ErrInvalid)
	}
	if err := checkPassword(password); err != nil {
		return nil, err
	}
	u := &model.User{ID: randID(), Username: username, DisplayName: strings.TrimSpace(display),
		PasswordHash: HashPassword(password), IsAdmin: isAdmin, CreatedAt: s.now()}
	if err := s.store.CreateUser(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

func checkPassword(pw string) error {
	if utf8.RuneCountInString(pw) < minPassword {
		return fmt.Errorf("%w: password must be at least %d characters", ErrInvalid, minPassword)
	}
	return nil
}

// ChangePassword 修改自己的密码，需要旧密码；成功后注销其他会话。
func (s *Service) ChangePassword(ctx context.Context, p *Principal, oldPw, newPw string) error {
	if !CheckPassword(p.User.PasswordHash, oldPw) {
		return ErrInvalidCredentials
	}
	return s.setPassword(ctx, p.User, newPw, p.Token.ID)
}

func (s *Service) setPassword(ctx context.Context, u *model.User, pw, keepToken string) error {
	if err := checkPassword(pw); err != nil {
		return err
	}
	u.PasswordHash = HashPassword(pw)
	if err := s.store.UpdateUser(ctx, u); err != nil {
		return err
	}
	sessions, err := s.store.ListTokens(ctx, u.ID, KindSession)
	if err != nil {
		return err
	}
	for _, t := range sessions {
		if t.ID != keepToken {
			_ = s.store.DeleteToken(ctx, t.ID)
		}
	}
	return nil
}

// MemberView 是租户成员列表项。
type MemberView struct {
	UserID      string     `json:"user_id"`
	Username    string     `json:"username"`
	DisplayName string     `json:"display_name,omitempty"`
	Role        model.Role `json:"role"`
	IsAdmin     bool       `json:"is_admin"`
}

func (s *Service) Members(ctx context.Context, p *Principal) ([]MemberView, error) {
	ms, err := s.store.ListMembers(ctx, p.TenantID)
	if err != nil {
		return nil, err
	}
	out := make([]MemberView, 0, len(ms))
	for _, m := range ms {
		u, err := s.store.GetUser(ctx, m.UserID)
		if err != nil {
			continue
		}
		out = append(out, MemberView{UserID: u.ID, Username: u.Username, DisplayName: u.DisplayName, Role: m.Role, IsAdmin: u.IsAdmin})
	}
	return out, nil
}

// AddMember 由租户管理员把用户加入当前租户；用户不存在且提供了密码时先创建用户。
func (s *Service) AddMember(ctx context.Context, p *Principal, username, display, password string, role model.Role) (*MemberView, error) {
	if !p.Can(model.RoleAdmin) {
		return nil, ErrForbidden
	}
	if role.Rank() == 0 {
		return nil, fmt.Errorf("%w: role must be viewer, developer or admin", ErrInvalid)
	}
	u, err := s.store.GetUserByName(ctx, username)
	if errors.Is(err, store.ErrNotFound) {
		if password == "" {
			return nil, fmt.Errorf("%w: user %s does not exist, provide a password to create it", ErrInvalid, username)
		}
		u, err = s.CreateUser(ctx, username, display, password, false)
	}
	if err != nil {
		return nil, err
	}
	if err := s.store.PutMembership(ctx, &model.Membership{TenantID: p.TenantID, UserID: u.ID, Role: role}); err != nil {
		return nil, err
	}
	return &MemberView{UserID: u.ID, Username: u.Username, DisplayName: u.DisplayName, Role: role, IsAdmin: u.IsAdmin}, nil
}

func (s *Service) SetMemberRole(ctx context.Context, p *Principal, userID string, role model.Role) error {
	if !p.Can(model.RoleAdmin) {
		return ErrForbidden
	}
	if role.Rank() == 0 {
		return fmt.Errorf("%w: role must be viewer, developer or admin", ErrInvalid)
	}
	if userID == p.User.ID && role != model.RoleAdmin && !p.User.IsAdmin {
		return fmt.Errorf("%w: cannot demote yourself", ErrInvalid)
	}
	if err := s.requireMember(ctx, p.TenantID, userID); err != nil {
		return err
	}
	return s.store.PutMembership(ctx, &model.Membership{TenantID: p.TenantID, UserID: userID, Role: role})
}

func (s *Service) RemoveMember(ctx context.Context, p *Principal, userID string) error {
	if !p.Can(model.RoleAdmin) {
		return ErrForbidden
	}
	if userID == p.User.ID {
		return fmt.Errorf("%w: cannot remove yourself", ErrInvalid)
	}
	return s.store.DeleteMembership(ctx, p.TenantID, userID)
}

// ResetPassword 由租户管理员重置本租户成员的密码（不能重置系统管理员，除非自己也是）。
func (s *Service) ResetPassword(ctx context.Context, p *Principal, userID, password string) error {
	if !p.Can(model.RoleAdmin) {
		return ErrForbidden
	}
	if err := s.requireMember(ctx, p.TenantID, userID); err != nil {
		return err
	}
	u, err := s.store.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	if u.IsAdmin && !p.User.IsAdmin {
		return ErrForbidden
	}
	return s.setPassword(ctx, u, password, "")
}

func (s *Service) requireMember(ctx context.Context, tenantID, userID string) error {
	ms, err := s.store.ListMembers(ctx, tenantID)
	if err != nil {
		return err
	}
	for _, m := range ms {
		if m.UserID == userID {
			return nil
		}
	}
	return store.ErrNotFound
}

// MaxAPITokenTTL 是 API Key 的最长有效期（不填为永不过期）。
const MaxAPITokenTTL = 3 * 365 * 24 * time.Hour

// CreateAPIToken 创建个人访问令牌（绑定当前租户；ttl 为 0 时永不过期），明文只返回一次。
func (s *Service) CreateAPIToken(ctx context.Context, p *Principal, name string, ttl time.Duration) (string, *model.Token, error) {
	if strings.TrimSpace(name) == "" {
		return "", nil, fmt.Errorf("%w: token name is required", ErrInvalid)
	}
	if ttl < 0 || ttl > MaxAPITokenTTL {
		return "", nil, fmt.Errorf("%w: expiry must be within 3 years", ErrInvalid)
	}
	return s.issue(ctx, p.User.ID, p.TenantID, KindAPI, truncate(strings.TrimSpace(name), 64), ttl)
}

// Sessions 返回用户未过期的登录会话。
func (s *Service) Sessions(ctx context.Context, p *Principal) ([]*model.Token, error) {
	ts, err := s.store.ListTokens(ctx, p.User.ID, KindSession)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := ts[:0]
	for _, t := range ts {
		if t.ExpiresAt == nil || t.ExpiresAt.After(now) {
			out = append(out, t)
		}
	}
	return out, nil
}

// RevokeSession 结束自己的某个会话（含当前会话）。
func (s *Service) RevokeSession(ctx context.Context, p *Principal, id string) error {
	ts, err := s.store.ListTokens(ctx, p.User.ID, KindSession)
	if err != nil {
		return err
	}
	for _, t := range ts {
		if t.ID == id {
			return s.store.DeleteToken(ctx, id)
		}
	}
	return store.ErrNotFound
}

// Profile 是可由用户自己修改的资料。
type Profile struct {
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
	Gender      string `json:"gender"`
	Bio         string `json:"bio"`
}

var emailRE = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// UpdateProfile 修改自己的姓名、邮箱、性别与简介。
func (s *Service) UpdateProfile(ctx context.Context, p *Principal, pr Profile) (*model.User, error) {
	pr.DisplayName, pr.Email, pr.Bio = strings.TrimSpace(pr.DisplayName), strings.TrimSpace(pr.Email), strings.TrimSpace(pr.Bio)
	switch {
	case utf8.RuneCountInString(pr.DisplayName) > 32:
		return nil, fmt.Errorf("%w: name must be at most 32 characters", ErrInvalid)
	case pr.Email != "" && (!emailRE.MatchString(pr.Email) || len(pr.Email) > 254):
		return nil, fmt.Errorf("%w: invalid email address", ErrInvalid)
	case pr.Gender != "" && pr.Gender != "male" && pr.Gender != "female":
		return nil, fmt.Errorf("%w: gender must be male, female or empty", ErrInvalid)
	case utf8.RuneCountInString(pr.Bio) > 500:
		return nil, fmt.Errorf("%w: bio must be at most 500 characters", ErrInvalid)
	}
	u, err := s.store.GetUser(ctx, p.User.ID)
	if err != nil {
		return nil, err
	}
	u.DisplayName, u.Email, u.Gender, u.Bio = pr.DisplayName, pr.Email, pr.Gender, pr.Bio
	if err := s.store.UpdateUser(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

// DeleteAccount 删除自己的账号（需要密码确认）；唯一的系统管理员不能删除。
// 会话、令牌、租户成员关系、流水线成员与收藏随之删除；创建的流水线保留。
func (s *Service) DeleteAccount(ctx context.Context, p *Principal, password string) error {
	u, err := s.store.GetUser(ctx, p.User.ID)
	if err != nil {
		return err
	}
	if !CheckPassword(u.PasswordHash, password) {
		return ErrInvalidCredentials
	}
	if u.IsAdmin {
		users, err := s.store.ListUsers(ctx)
		if err != nil {
			return err
		}
		admins := 0
		for _, o := range users {
			if o.IsAdmin {
				admins++
			}
		}
		if admins <= 1 {
			return fmt.Errorf("%w: the only system administrator cannot be deleted", ErrInvalid)
		}
	}
	return s.store.DeleteUser(ctx, u.ID)
}

func (s *Service) APITokens(ctx context.Context, p *Principal) ([]*model.Token, error) {
	return s.store.ListTokens(ctx, p.User.ID, KindAPI)
}

func (s *Service) DeleteAPIToken(ctx context.Context, p *Principal, id string) error {
	ts, err := s.store.ListTokens(ctx, p.User.ID, KindAPI)
	if err != nil {
		return err
	}
	for _, t := range ts {
		if t.ID == id {
			return s.store.DeleteToken(ctx, id)
		}
	}
	return store.ErrNotFound
}

// Cleanup 删除过期会话，由后台循环调用。
func (s *Service) Cleanup(ctx context.Context) error {
	return s.store.DeleteExpiredTokens(ctx, s.now())
}

func randID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
