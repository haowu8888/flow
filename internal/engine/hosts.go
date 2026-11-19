package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"strings"
	"time"
	"unicode"

	"golang.org/x/crypto/ssh"

	"flow/internal/model"
	"flow/internal/pipeline"
	"flow/internal/protocol"
	"flow/internal/sshx"
	"flow/internal/store"
)

const maxNameLen = 64

func checkName(kind, name string) error {
	if name == "" || len([]rune(name)) > maxNameLen || strings.ContainsFunc(name, unicode.IsControl) {
		return fmt.Errorf("%w: %s name is required (max %d characters)", ErrInvalid, kind, maxNameLen)
	}
	return nil
}

// ---- 环境 ----

// EnvironmentInput 是创建 / 更新环境的可编辑字段。
type EnvironmentInput struct {
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	Rank            int      `json:"rank"`
	RequireApproval bool     `json:"require_approval"`
	Approvers       []string `json:"approvers"`
	// Variables 整体替换；为 nil 时保持不变。
	Variables map[string]string `json:"variables"`
	// Secrets 新增或覆盖的密钥（明文，只写）；RemoveSecrets 要删除的密钥名。
	Secrets       map[string]string `json:"secrets"`
	RemoveSecrets []string          `json:"remove_secrets"`
}

const maxEnvVars = 200

func checkEnvVarNames(kind string, names []string) error {
	if len(names) > maxEnvVars {
		return fmt.Errorf("%w: too many %s", ErrInvalid, kind)
	}
	for _, n := range names {
		if !pipeline.ValidEnvName(n) || strings.HasPrefix(strings.ToUpper(n), "FLOW_") {
			return fmt.Errorf("%w: %s name %q must be a valid variable name without the FLOW_ prefix", ErrInvalid, kind, n)
		}
	}
	return nil
}

func envSecretAAD(tenantID, envID, name string) string {
	return "env/" + tenantID + "/" + envID + "/" + name
}

// applyEnvSecrets 写入环境的变量与密钥变更（密钥加密保存）。
func (e *Engine) applyEnvSecrets(env *model.Environment, in EnvironmentInput) error {
	if in.Variables != nil {
		if err := checkEnvVarNames("variable", slices.Collect(maps.Keys(in.Variables))); err != nil {
			return err
		}
		env.Variables = in.Variables
	}
	if err := checkEnvVarNames("secret", slices.Collect(maps.Keys(in.Secrets))); err != nil {
		return err
	}
	if env.Secrets == nil {
		env.Secrets = map[string][]byte{}
	}
	for _, n := range in.RemoveSecrets {
		delete(env.Secrets, n)
	}
	for n, v := range in.Secrets {
		env.Secrets[n] = e.cipher.Seal([]byte(v), envSecretAAD(env.TenantID, env.ID, n))
	}
	if len(env.Secrets) > maxEnvVars {
		return fmt.Errorf("%w: too many secrets", ErrInvalid)
	}
	return nil
}

// withSecretNames 填充返回给接口的密钥名（不含值）。
func withSecretNames(env *model.Environment) *model.Environment {
	env.SecretNames = slices.Sorted(maps.Keys(env.Secrets))
	if env.SecretNames == nil {
		env.SecretNames = []string{}
	}
	return env
}

// envInjection 解密环境的密钥，返回注入部署任务的变量与密钥。
func (e *Engine) envInjection(env *model.Environment) (vars, secrets map[string]string, err error) {
	secrets = map[string]string{}
	var errs []error
	for n, c := range env.Secrets {
		plain, err := e.cipher.Open(c, envSecretAAD(env.TenantID, env.ID, n))
		if err != nil {
			errs = append(errs, fmt.Errorf("environment secret %s: %w", n, err))
			continue
		}
		secrets[n] = string(plain)
	}
	return env.Variables, secrets, errors.Join(errs...)
}

func (in *EnvironmentInput) normalize() error {
	in.Name, in.Description = strings.TrimSpace(in.Name), strings.TrimSpace(in.Description)
	approvers := in.Approvers[:0:0]
	for _, a := range in.Approvers {
		if a = strings.TrimSpace(a); a != "" && !slices.Contains(approvers, a) {
			approvers = append(approvers, a)
		}
	}
	in.Approvers = approvers
	return checkName("environment", in.Name)
}

func (e *Engine) Environments(ctx context.Context, tenantID string) ([]*model.Environment, error) {
	list, err := e.store.ListEnvironments(ctx, tenantID)
	for _, env := range list {
		withSecretNames(env)
	}
	return list, err
}

func (e *Engine) Environment(ctx context.Context, id string) (*model.Environment, error) {
	env, err := e.store.GetEnvironment(ctx, id)
	if err != nil {
		return nil, err
	}
	return withSecretNames(env), nil
}

func (e *Engine) CreateEnvironment(ctx context.Context, tenantID string, in EnvironmentInput) (*model.Environment, error) {
	if err := in.normalize(); err != nil {
		return nil, err
	}
	now := e.now()
	env := &model.Environment{ID: newID(), TenantID: tenantID, Name: in.Name, Description: in.Description, Rank: in.Rank,
		RequireApproval: in.RequireApproval, Approvers: in.Approvers, CreatedAt: now, UpdatedAt: now}
	if err := e.applyEnvSecrets(env, in); err != nil {
		return nil, err
	}
	if err := e.store.CreateEnvironment(ctx, env); err != nil {
		return nil, nameConflict(err, "environment", in.Name)
	}
	return withSecretNames(env), nil
}

func (e *Engine) UpdateEnvironment(ctx context.Context, id string, in EnvironmentInput) (*model.Environment, error) {
	if err := in.normalize(); err != nil {
		return nil, err
	}
	env, err := e.store.GetEnvironment(ctx, id)
	if err != nil {
		return nil, err
	}
	env.Name, env.Description, env.Rank, env.RequireApproval, env.Approvers, env.UpdatedAt =
		in.Name, in.Description, in.Rank, in.RequireApproval, in.Approvers, e.now()
	if err := e.applyEnvSecrets(env, in); err != nil {
		return nil, err
	}
	if err := e.store.UpdateEnvironment(ctx, env); err != nil {
		return nil, nameConflict(err, "environment", in.Name)
	}
	return withSecretNames(env), nil
}

func (e *Engine) DeleteEnvironment(ctx context.Context, id string) error {
	return e.store.DeleteEnvironment(ctx, id)
}

func nameConflict(err error, kind, name string) error {
	if errors.Is(err, store.ErrConflict) {
		return fmt.Errorf("%w: %s %q already exists", store.ErrConflict, kind, name)
	}
	return err
}

// ---- 主机组 ----

type HostGroupInput struct {
	Name          string `json:"name"`
	Description   string `json:"description"`
	EnvironmentID string `json:"environment_id"`
}

func (e *Engine) HostGroups(ctx context.Context, tenantID string) ([]*model.HostGroup, error) {
	return e.store.ListHostGroups(ctx, tenantID)
}

func (e *Engine) HostGroup(ctx context.Context, id string) (*model.HostGroup, error) {
	return e.store.GetHostGroup(ctx, id)
}

func (e *Engine) checkGroupInput(ctx context.Context, tenantID string, in *HostGroupInput) error {
	in.Name, in.Description = strings.TrimSpace(in.Name), strings.TrimSpace(in.Description)
	if err := checkName("host group", in.Name); err != nil {
		return err
	}
	if strings.ContainsAny(in.Name, " \t\"'#:{}[],") {
		return fmt.Errorf("%w: host group name is referenced in YAML and must not contain spaces, quotes or : # , [ ] { }", ErrInvalid)
	}
	if in.EnvironmentID != "" {
		env, err := e.store.GetEnvironment(ctx, in.EnvironmentID)
		if err != nil || env.TenantID != tenantID {
			return fmt.Errorf("%w: environment not found", ErrInvalid)
		}
	}
	return nil
}

func (e *Engine) CreateHostGroup(ctx context.Context, tenantID string, in HostGroupInput) (*model.HostGroup, error) {
	if err := e.checkGroupInput(ctx, tenantID, &in); err != nil {
		return nil, err
	}
	now := e.now()
	g := &model.HostGroup{ID: newID(), TenantID: tenantID, Name: in.Name, Description: in.Description,
		EnvironmentID: in.EnvironmentID, CreatedAt: now, UpdatedAt: now}
	if err := e.store.CreateHostGroup(ctx, g); err != nil {
		return nil, nameConflict(err, "host group", in.Name)
	}
	return g, nil
}

// UpdateHostGroup 更新主机组；被流水线引用时不能改名（流水线以名称引用）。
func (e *Engine) UpdateHostGroup(ctx context.Context, id string, in HostGroupInput) (*model.HostGroup, error) {
	g, err := e.store.GetHostGroup(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := e.checkGroupInput(ctx, g.TenantID, &in); err != nil {
		return nil, err
	}
	if in.Name != g.Name {
		if users, err := e.HostGroupUsers(ctx, g.TenantID, g.Name); err != nil {
			return nil, err
		} else if len(users) > 0 {
			return nil, fmt.Errorf("%w: host group %q is used by pipeline %q; update the pipelines before renaming", ErrInvalid, g.Name, users[0].Name)
		}
	}
	g.Name, g.Description, g.EnvironmentID, g.UpdatedAt = in.Name, in.Description, in.EnvironmentID, e.now()
	if err := e.store.UpdateHostGroup(ctx, g); err != nil {
		return nil, nameConflict(err, "host group", in.Name)
	}
	return g, nil
}

func (e *Engine) DeleteHostGroup(ctx context.Context, id string) error {
	g, err := e.store.GetHostGroup(ctx, id)
	if err != nil {
		return err
	}
	if users, err := e.HostGroupUsers(ctx, g.TenantID, g.Name); err != nil {
		return err
	} else if len(users) > 0 {
		return fmt.Errorf("%w: host group %q is used by pipeline %q", ErrInvalid, g.Name, users[0].Name)
	}
	return e.store.DeleteHostGroup(ctx, id)
}

// HostGroupUsers 返回部署到该主机组的流水线。
func (e *Engine) HostGroupUsers(ctx context.Context, tenantID, name string) ([]*model.Pipeline, error) {
	pipes, err := e.store.ListPipelines(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := []*model.Pipeline{}
	for _, p := range pipes {
		if spec, err := pipeline.Parse([]byte(p.YAML)); err == nil && slices.Contains(spec.HostGroups(), name) {
			out = append(out, p)
		}
	}
	return out, nil
}

// checkHostGroups 校验流水线引用的主机组存在（保存与创建运行时）。
func checkHostGroups(ctx context.Context, get func(context.Context, string, string) (*model.HostGroup, error), tenantID string, spec *pipeline.Spec) error {
	for _, name := range spec.HostGroups() {
		if _, err := get(ctx, tenantID, name); errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: host group %q not found; create it under 环境与主机 first", ErrInvalid, name)
		} else if err != nil {
			return err
		}
	}
	return nil
}

// ---- 主机 ----

// HostInput 是主机的可编辑字段；Credential 为私钥或密码明文，更新时为空表示保持不变。
type HostInput struct {
	Name        string   `json:"name"`
	Address     string   `json:"address"`
	Port        int      `json:"port"`
	Username    string   `json:"username"`
	AuthType    string   `json:"auth_type"`
	Credential  string   `json:"credential"`
	Description string   `json:"description"`
	GroupIDs    []string `json:"group_ids"`
	JumpHostID  string   `json:"jump_host_id"`
	Mode        string   `json:"mode"` // ssh（默认）/ agent，创建后不可修改
}

func hostAAD(tenantID, id string) string { return "host/" + tenantID + "/" + id }

func (e *Engine) checkHostInput(ctx context.Context, tenantID string, in *HostInput, creating bool) error {
	in.Name, in.Address, in.Username, in.Description = strings.TrimSpace(in.Name), strings.TrimSpace(in.Address),
		strings.TrimSpace(in.Username), strings.TrimSpace(in.Description)
	if in.Name == "" {
		in.Name = in.Address
	}
	if err := checkName("host", in.Name); err != nil {
		return err
	}
	if in.Mode == model.HostModeAgent {
		// Agent 主动连接 Server：不需要地址与 SSH 凭据。
		in.Port, in.Username, in.AuthType, in.Credential, in.JumpHostID = 0, "", "", "", ""
		return e.checkHostGroups(ctx, tenantID, in)
	}
	in.Mode = model.HostModeSSH
	if in.Address == "" || strings.ContainsAny(in.Address, " \t/@") || len(in.Address) > 253 {
		return fmt.Errorf("%w: address must be a hostname or IP", ErrInvalid)
	}
	if in.Port == 0 {
		in.Port = 22
	}
	if in.Port < 1 || in.Port > 65535 {
		return fmt.Errorf("%w: port must be between 1 and 65535", ErrInvalid)
	}
	if in.Username == "" || strings.ContainsAny(in.Username, " \t@:") {
		return fmt.Errorf("%w: username is required", ErrInvalid)
	}
	switch in.AuthType {
	case model.HostAuthKey:
		if in.Credential != "" {
			if _, err := ssh.ParsePrivateKey([]byte(in.Credential)); err != nil {
				var pass *ssh.PassphraseMissingError
				if errors.As(err, &pass) {
					return fmt.Errorf("%w: private keys protected by a passphrase are not supported", ErrInvalid)
				}
				return fmt.Errorf("%w: invalid private key: %v", ErrInvalid, err)
			}
		}
	case model.HostAuthPassword:
	default:
		return fmt.Errorf("%w: auth_type must be key or password", ErrInvalid)
	}
	if creating && in.Credential == "" {
		return fmt.Errorf("%w: a private key or password is required", ErrInvalid)
	}
	if err := e.checkHostGroups(ctx, tenantID, in); err != nil {
		return err
	}
	if in.JumpHostID != "" {
		j, err := e.store.GetHost(ctx, in.JumpHostID)
		if err != nil || j.TenantID != tenantID {
			return fmt.Errorf("%w: jump host not found", ErrInvalid)
		}
		if j.JumpHostID != "" {
			return fmt.Errorf("%w: jump host %s itself goes through another jump host; only one level is supported", ErrInvalid, j.Name)
		}
		if j.Mode == model.HostModeAgent {
			return fmt.Errorf("%w: an agent host cannot be a jump host", ErrInvalid)
		}
	}
	return nil
}

func (e *Engine) checkHostGroups(ctx context.Context, tenantID string, in *HostInput) error {
	groups := in.GroupIDs[:0:0]
	for _, id := range in.GroupIDs {
		g, err := e.store.GetHostGroup(ctx, id)
		if err != nil || g.TenantID != tenantID {
			return fmt.Errorf("%w: host group %s not found", ErrInvalid, id)
		}
		if !slices.Contains(groups, id) {
			groups = append(groups, id)
		}
	}
	in.GroupIDs = groups
	return nil
}

func (e *Engine) Hosts(ctx context.Context, tenantID string) ([]*model.Host, error) {
	list, err := e.store.ListHosts(ctx, tenantID)
	if err == nil {
		e.withAgents(ctx, tenantID, list)
	}
	return list, err
}

func (e *Engine) Host(ctx context.Context, id string) (*model.Host, error) {
	h, err := e.store.GetHost(ctx, id)
	if err == nil {
		e.withAgents(ctx, h.TenantID, []*model.Host{h})
	}
	return h, err
}

func (e *Engine) CreateHost(ctx context.Context, tenantID string, in HostInput) (*model.Host, error) {
	if err := e.checkHostInput(ctx, tenantID, &in, true); err != nil {
		return nil, err
	}
	now := e.now()
	h := &model.Host{ID: newID(), TenantID: tenantID, Name: in.Name, Address: in.Address, Port: in.Port, Username: in.Username,
		AuthType: in.AuthType, Description: in.Description, GroupIDs: in.GroupIDs, JumpHostID: in.JumpHostID, Mode: in.Mode, CreatedAt: now, UpdatedAt: now}
	h.Credential = []byte{}
	if in.Mode != model.HostModeAgent {
		h.Credential = e.cipher.Seal([]byte(in.Credential), hostAAD(tenantID, h.ID))
	}
	if err := e.store.CreateHost(ctx, h); err != nil {
		return nil, nameConflict(err, "host", in.Name)
	}
	return h, nil
}

// UpdateHost 更新主机；地址或端口变化时清除已信任的主机公钥，下次连接重新记录。
func (e *Engine) UpdateHost(ctx context.Context, id string, in HostInput) (*model.Host, error) {
	h, err := e.store.GetHost(ctx, id)
	if err != nil {
		return nil, err
	}
	in.Mode = h.Mode
	if h.Mode != model.HostModeAgent && in.AuthType != h.AuthType && in.Credential == "" {
		return nil, fmt.Errorf("%w: enter the new credential when changing the auth type", ErrInvalid)
	}
	if err := e.checkHostInput(ctx, h.TenantID, &in, false); err != nil {
		return nil, err
	}
	if in.JumpHostID == id {
		return nil, fmt.Errorf("%w: a host cannot be its own jump host", ErrInvalid)
	}
	if in.JumpHostID != "" {
		if users, err := e.jumpUsers(ctx, h); err != nil {
			return nil, err
		} else if len(users) > 0 {
			return nil, fmt.Errorf("%w: %s is the jump host of %s and cannot go through another jump host", ErrInvalid, h.Name, users[0].Name)
		}
	}
	if in.Address != h.Address || in.Port != h.Port || in.JumpHostID != h.JumpHostID {
		h.HostKey, h.CheckedAt, h.CheckError = "", nil, ""
	}
	h.Name, h.Address, h.Port, h.Username, h.AuthType, h.Description, h.GroupIDs, h.JumpHostID, h.UpdatedAt =
		in.Name, in.Address, in.Port, in.Username, in.AuthType, in.Description, in.GroupIDs, in.JumpHostID, e.now()
	if in.Credential != "" {
		h.Credential = e.cipher.Seal([]byte(in.Credential), hostAAD(h.TenantID, h.ID))
	}
	if err := e.store.UpdateHost(ctx, h); err != nil {
		return nil, nameConflict(err, "host", in.Name)
	}
	return h, nil
}

func (e *Engine) DeleteHost(ctx context.Context, id string) error {
	h, err := e.store.GetHost(ctx, id)
	if err != nil {
		return err
	}
	if users, err := e.jumpUsers(ctx, h); err != nil {
		return err
	} else if len(users) > 0 {
		return fmt.Errorf("%w: %s is the jump host of %s", ErrInvalid, h.Name, users[0].Name)
	}
	if r, err := e.hostAgent(ctx, h); err == nil {
		if err := e.store.DeleteRunner(ctx, r.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	return e.store.DeleteHost(ctx, id)
}

// jumpUsers 返回以 h 为跳板机的主机。
func (e *Engine) jumpUsers(ctx context.Context, h *model.Host) ([]*model.Host, error) {
	hosts, err := e.store.ListHosts(ctx, h.TenantID)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(hosts, func(x *model.Host) bool { return x.JumpHostID != h.ID }), nil
}

// sshTarget 解密凭据，配置了跳板机时一并解析。
func (e *Engine) sshTarget(ctx context.Context, h *model.Host) (sshx.Target, error) {
	t, err := e.directTarget(h)
	if err != nil || h.JumpHostID == "" {
		return t, err
	}
	j, err := e.store.GetHost(ctx, h.JumpHostID)
	if err != nil {
		return t, fmt.Errorf("jump host: %w", err)
	}
	jt, err := e.directTarget(j)
	if err != nil {
		return t, fmt.Errorf("jump host %s: %w", j.Name, err)
	}
	t.Jump = &jt
	return t, nil
}

func (e *Engine) directTarget(h *model.Host) (sshx.Target, error) {
	plain, err := e.cipher.Open(h.Credential, hostAAD(h.TenantID, h.ID))
	if err != nil {
		return sshx.Target{}, err
	}
	t := sshx.Target{Address: h.Address, Port: h.Port, User: h.Username, HostKey: h.HostKey}
	if h.AuthType == model.HostAuthPassword {
		t.Password = string(plain)
	} else {
		t.PrivateKey = string(plain)
	}
	return t, nil
}

func deployHost(name string, t sshx.Target) protocol.DeployHost {
	d := protocol.DeployHost{Name: name, Address: t.Address, Port: t.Port, User: t.User, Password: t.Password,
		PrivateKey: t.PrivateKey, HostKey: t.HostKey}
	if t.Jump != nil {
		j := deployHost("", *t.Jump)
		d.Jump = &j
	}
	return d
}

// HostCheck 是连接测试的结果。
type HostCheck struct {
	Host        *model.Host `json:"host"`
	OK          bool        `json:"ok"`
	Error       string      `json:"error,omitempty"`
	Output      string      `json:"output,omitempty"`       // 主机信息（uname 等）
	Fingerprint string      `json:"fingerprint,omitempty"`  // 主机公钥指纹
	NewHostKey  bool        `json:"new_host_key,omitempty"` // 本次首次记录了主机公钥
}

// TestHost 从 Server 连接主机并执行简单命令；首次成功时记录主机公钥（之后部署时校验）。
func (e *Engine) TestHost(ctx context.Context, id string) (*HostCheck, error) {
	h, err := e.store.GetHost(ctx, id)
	if err != nil {
		return nil, err
	}
	res := &HostCheck{Host: h}
	if h.Mode == model.HostModeAgent {
		return e.testAgent(ctx, h, res)
	}
	t, err := e.sshTarget(ctx, h)
	if err == nil {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		var c *sshx.Client
		if c, err = sshx.Dial(ctx, t); err == nil {
			var out strings.Builder
			_, err = c.Run(ctx, `echo "$(whoami)@$(hostname) · $(uname -sr)"; (df -h . | tail -1) 2>/dev/null || true`, nil, &out)
			res.Output = strings.TrimSpace(out.String())
			if h.HostKey == "" && c.HostKey != "" {
				h.HostKey, res.NewHostKey = c.HostKey, true
			}
			c.Close()
		}
	}
	now := e.now()
	h.CheckedAt, h.CheckError = &now, ""
	if err != nil {
		h.CheckError = hostError(err)
		res.Error = h.CheckError
	}
	res.OK = err == nil
	res.Fingerprint = sshx.Fingerprint(h.HostKey)
	if err := e.store.UpdateHost(ctx, h); err != nil {
		return nil, err
	}
	return res, nil
}

// hostError 把常见连接错误转成可操作的说明。
func hostError(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, sshx.ErrHostKeyMismatch):
		return "主机公钥与之前记录的不一致（主机重装过，或网络中存在中间人）。确认无误后可「重置指纹」。" + err.Error()
	case strings.Contains(err.Error(), "unable to authenticate"):
		return "认证失败：用户名、私钥或密码不正确。" + err.Error()
	case strings.Contains(err.Error(), "connection refused"):
		return "连接被拒绝：SSH 服务未启动或端口不正确。" + err.Error()
	case errors.As(err, &ne) && ne.Timeout():
		return "连接超时：地址不可达或被防火墙拦截（测试从 Flow 服务端发起，部署从 Runner 发起）。" + err.Error()
	}
	return err.Error()
}

// ResetHostKey 清除已信任的主机公钥（主机重装后），下次连接测试时重新记录。
func (e *Engine) ResetHostKey(ctx context.Context, id string) (*model.Host, error) {
	h, err := e.store.GetHost(ctx, id)
	if err != nil {
		return nil, err
	}
	h.HostKey, h.CheckedAt, h.CheckError, h.UpdatedAt = "", nil, "", e.now()
	return h, e.store.UpdateHost(ctx, h)
}

// ---- 部署 ----

// resolveDeploy 为部署任务解析目标主机与凭据（领取时调用）。返回的错误信息交给 Runner 写入日志并失败。
func (e *Engine) resolveDeploy(ctx context.Context, j *model.Job) ([]protocol.DeployHost, *model.HostGroup, string) {
	g, err := e.store.GetHostGroupByName(ctx, j.TenantID, j.Deploy.Group)
	if err != nil {
		return nil, nil, fmt.Sprintf("host group %q: %v", j.Deploy.Group, err)
	}
	hosts, err := e.store.ListHosts(ctx, j.TenantID)
	if err != nil {
		return nil, g, err.Error()
	}
	var out []protocol.DeployHost
	for _, h := range hosts {
		if !slices.Contains(h.GroupIDs, g.ID) {
			continue
		}
		if h.Mode == model.HostModeAgent {
			out = append(out, protocol.DeployHost{Name: h.Name, Agent: true})
			continue
		}
		t, err := e.sshTarget(ctx, h)
		if err != nil {
			return nil, g, fmt.Sprintf("host %s: %v", h.Name, err)
		}
		out = append(out, deployHost(h.Name, t))
	}
	if len(out) == 0 {
		return nil, g, fmt.Sprintf("host group %q has no hosts", g.Name)
	}
	return out, g, ""
}

// recordDeployment 在部署任务开始执行时写入部署记录。
func (e *Engine) recordDeployment(ctx context.Context, j *model.Job, g *model.HostGroup, hosts []protocol.DeployHost, art *model.DeployedArtifact) {
	run, err := e.store.GetRun(ctx, j.RunID)
	if err != nil {
		e.logf("record deployment %s: %v", j.ID, err)
		return
	}
	p, err := e.store.GetPipeline(ctx, run.PipelineID)
	if err != nil {
		e.logf("record deployment %s: %v", j.ID, err)
		return
	}
	d := &model.Deployment{ID: newID(), TenantID: j.TenantID, PipelineID: p.ID, PipelineName: p.Name, RunID: run.ID,
		RunNumber: run.Number, JobID: j.ID, JobName: j.Name, GroupName: j.Deploy.Group, Commit: run.Commit,
		Status: model.StatusRunning, CreatedBy: run.CreatedBy, StartedAt: e.now(), Artifact: art}
	for _, h := range hosts {
		d.Hosts = append(d.Hosts, h.Name)
	}
	if g != nil && g.EnvironmentID != "" {
		if env, err := e.store.GetEnvironment(ctx, g.EnvironmentID); err == nil {
			d.EnvironmentID, d.EnvironmentName = env.ID, env.Name
		}
	}
	if err := e.store.CreateDeployment(ctx, d); err != nil {
		e.logf("record deployment %s: %v", j.ID, err)
	}
}

// namedArtifact 在上游命名构建物中按名称查找要部署的构建物（多个上游同名时取最后一个，与「下载构建物」一致）。
func namedArtifact(named []*model.Artifact, name string) *model.DeployedArtifact {
	var found *model.Artifact
	for _, a := range named {
		if a.Name == name {
			found = a
		}
	}
	if found == nil {
		return nil
	}
	return &model.DeployedArtifact{ID: found.ID, Name: found.Name, Version: found.Version, Format: found.Format, Size: found.Size}
}

func namedArtifactMissing(named []*model.Artifact, name string) string {
	var names []string
	for _, a := range named {
		if !slices.Contains(names, a.Name) {
			names = append(names, a.Name)
		}
	}
	if len(names) == 0 {
		return fmt.Sprintf("deploy.artifact: 上游任务没有上传构建物 %q（在上游任务中添加「构建物上传」步骤，名称填 %s）", name, name)
	}
	return fmt.Sprintf("deploy.artifact: 上游任务没有名为 %q 的构建物（可用：%s）", name, strings.Join(names, "、"))
}

func (e *Engine) Deployments(ctx context.Context, tenantID string, f store.DeploymentFilter) ([]*model.Deployment, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	return e.store.ListDeployments(ctx, tenantID, f)
}

// testAgent 检查 Agent 是否在线（Agent 每次轮询都会上报）。
func (e *Engine) testAgent(ctx context.Context, h *model.Host, res *HostCheck) (*HostCheck, error) {
	e.withAgents(ctx, h.TenantID, []*model.Host{h})
	now := e.now()
	h.CheckedAt, h.CheckError = &now, ""
	switch {
	case h.AgentSeenAt == nil:
		h.CheckError = "Agent 尚未连接：在主机上执行接入命令后再测试。"
	case now.Sub(*h.AgentSeenAt) > agentOnline:
		h.CheckError = "Agent 离线：最近一次上报于 " + h.AgentSeenAt.Format(time.DateTime) + "，检查主机上的 flow-agent 服务。"
	default:
		res.Output = fmt.Sprintf("%s · %s/%s · flow-runner %s", h.AgentInfo.Hostname, h.AgentInfo.OS, h.AgentInfo.Arch, h.AgentInfo.Version)
	}
	res.OK, res.Error = h.CheckError == "", h.CheckError
	return res, e.store.UpdateHost(ctx, h)
}
