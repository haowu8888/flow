package engine

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"flow/internal/connect"
	"flow/internal/model"
	"flow/internal/pipeline"
	"flow/internal/steps"
	"flow/internal/store"
)

// ---- 通用变量组 ----

// SecretMask 是私密变量在编辑时的占位值：提交这个值（或空值）表示保持原值不变。
const SecretMask = "••••••"

const maxGroupVars = 200

func groupVarAAD(tenantID, groupID, name string) string {
	return "vargroup/" + tenantID + "/" + groupID + "/" + name
}

func (e *Engine) sealString(plain, aad string) string {
	return base64.StdEncoding.EncodeToString(e.cipher.Seal([]byte(plain), aad))
}

func (e *Engine) openString(sealed, aad string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return "", err
	}
	plain, err := e.cipher.Open(data, aad)
	return string(plain), err
}

// redactGroup 清空私密变量的值（接口不回显）。
func redactGroup(g *model.VarGroup) *model.VarGroup {
	for i := range g.Vars {
		if g.Vars[i].Secret {
			g.Vars[i].Value = ""
		}
	}
	return g
}

func (e *Engine) VarGroups(ctx context.Context, tenantID string) ([]*model.VarGroup, error) {
	list, err := e.store.ListVarGroups(ctx, tenantID)
	for _, g := range list {
		redactGroup(g)
	}
	return list, err
}

func (e *Engine) VarGroup(ctx context.Context, id string) (*model.VarGroup, error) {
	g, err := e.store.GetVarGroup(ctx, id)
	if err != nil {
		return nil, err
	}
	return redactGroup(g), nil
}

// SaveVarGroup 新建（id 为空）或更新变量组。私密变量提交空值或 SecretMask 时保留原来的值。
func (e *Engine) SaveVarGroup(ctx context.Context, tenantID, id, user string, in model.VarGroup) (*model.VarGroup, error) {
	in.Name, in.Description = strings.TrimSpace(in.Name), strings.TrimSpace(in.Description)
	if in.Name == "" || utf8.RuneCountInString(in.Name) > 64 || utf8.RuneCountInString(in.Description) > 200 {
		return nil, fmt.Errorf("%w: group name is required (at most 64 characters, description at most 200)", ErrInvalid)
	}
	if len(in.Vars) > maxGroupVars {
		return nil, fmt.Errorf("%w: at most %d variables per group", ErrInvalid, maxGroupVars)
	}
	now := e.now()
	g := &model.VarGroup{ID: id, TenantID: tenantID, Name: in.Name, Description: in.Description, CreatedBy: user, UpdatedBy: user, CreatedAt: now, UpdatedAt: now}
	var old *model.VarGroup
	if id != "" {
		var err error
		if old, err = e.store.GetVarGroup(ctx, id); err != nil {
			return nil, err
		}
		if old.TenantID != tenantID {
			return nil, store.ErrNotFound
		}
		g.CreatedBy, g.CreatedAt = old.CreatedBy, old.CreatedAt
	} else {
		g.ID = "vg-" + randHex(8)
	}
	seen := map[string]bool{}
	for _, v := range in.Vars {
		v.Name = strings.TrimSpace(v.Name)
		if !pipeline.ValidEnvName(v.Name) {
			return nil, fmt.Errorf("%w: %q is not a valid variable name", ErrInvalid, v.Name)
		}
		if seen[v.Name] {
			return nil, fmt.Errorf("%w: duplicate variable %q", ErrInvalid, v.Name)
		}
		seen[v.Name] = true
		if len(v.Value) > 64<<10 {
			return nil, fmt.Errorf("%w: value of %s is too long", ErrInvalid, v.Name)
		}
		if v.Secret {
			if v.Value == "" || v.Value == SecretMask {
				i := -1
				if old != nil {
					i = slices.IndexFunc(old.Vars, func(o model.GroupVar) bool { return o.Name == v.Name && o.Secret })
				}
				if i < 0 {
					return nil, fmt.Errorf("%w: secret variable %s needs a value", ErrInvalid, v.Name)
				}
				v.Value = old.Vars[i].Value
			} else {
				v.Value = e.sealString(v.Value, groupVarAAD(tenantID, g.ID, v.Name))
			}
		}
		g.Vars = append(g.Vars, v)
	}
	if err := e.store.PutVarGroup(ctx, g); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, fmt.Errorf("%w: a variable group named %q already exists", store.ErrConflict, g.Name)
		}
		return nil, err
	}
	return redactGroup(g), nil
}

func (e *Engine) DeleteVarGroup(ctx context.Context, tenantID, id string) error {
	g, err := e.store.GetVarGroup(ctx, id)
	if err != nil {
		return err
	}
	if g.TenantID != tenantID {
		return store.ErrNotFound
	}
	return e.store.DeleteVarGroup(ctx, id)
}

// runVarGroups 返回运行要注入的变量组（按创建顺序）：names 为空时全部，否则只取所列名称（不存在的报错）。
func (e *Engine) runVarGroups(ctx context.Context, tenantID string, names []string) ([]*model.VarGroup, error) {
	all, err := e.store.ListVarGroups(ctx, tenantID)
	if err != nil || len(names) == 0 {
		return all, err
	}
	var out []*model.VarGroup
	for _, g := range all {
		if slices.Contains(names, g.Name) {
			out = append(out, g)
		}
	}
	for _, n := range names {
		if !slices.ContainsFunc(out, func(g *model.VarGroup) bool { return g.Name == n }) {
			return nil, fmt.Errorf("%w: variable group %q does not exist", ErrInvalid, n)
		}
	}
	return out, nil
}

// groupPlainVars 合并变量组中的非私密变量（后创建的组覆盖先创建的）。
func groupPlainVars(groups []*model.VarGroup) map[string]string {
	out := map[string]string{}
	for _, g := range groups {
		for _, v := range g.Vars {
			if !v.Secret {
				out[v.Name] = v.Value
			}
		}
	}
	return out
}

// groupSecrets 解密 Job 关联的变量组中的私密变量。
func (e *Engine) groupSecrets(ctx context.Context, j *model.Job) (map[string]string, error) {
	out := map[string]string{}
	var errs []error
	for _, id := range j.VarGroups {
		g, err := e.store.GetVarGroup(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			continue // 运行开始后被删除
		}
		if err != nil {
			return out, err
		}
		for _, v := range g.Vars {
			if !v.Secret {
				continue
			}
			plain, err := e.openString(v.Value, groupVarAAD(g.TenantID, g.ID, v.Name))
			if err != nil {
				errs = append(errs, fmt.Errorf("%s/%s: %w", g.Name, v.Name, err))
				continue
			}
			out[v.Name] = plain
		}
	}
	return out, errors.Join(errs...)
}

// ---- 服务连接 ----

// ConnField 描述服务连接类型的一个字段。
type ConnField struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Secret      bool     `json:"secret,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Description string   `json:"description,omitempty"`
	Options     []string `json:"options,omitempty"` // 下拉选项（值）
	Bool        bool     `json:"bool,omitempty"`    // 开关，值为 true / false
}

// ConnType 是服务连接类型。
type ConnType struct {
	Type        string      `json:"type"`
	Label       string      `json:"label"`
	Description string      `json:"description"`
	Fields      []ConnField `json:"fields"`
	Testable    bool        `json:"testable,omitempty"`
}

// ConnTypes 是支持的服务连接类型（字段的 Key 以大写形式注入步骤：CONN_<KEY>）。
var ConnTypes = []ConnType{
	{Type: "git", Label: "Git", Description: "Git 仓库的 HTTPS 凭据：通用（任意服务，账号 + 令牌）；Gitea、码云、GitHub 还能在流水线源里直接下拉选仓库和分支", Testable: true, Fields: []ConnField{
		{Key: "platform", Label: "平台", Options: []string{"generic", "gitea", "gitee", "github"}, Description: "通用：只用来克隆 / 推送；选具体平台还能在流水线源里下拉选仓库"},
		{Key: "server", Label: "站点地址", Placeholder: "https://gitea.example.com", Description: "Gitea 必填；其他平台留空"},
		{Key: "username", Label: "用户名", Required: true},
		{Key: "token", Label: "令牌 / 密码", Secret: true, Required: true, Description: "GitHub：Personal access token（打标签 / 推送要读写权限）；Gitea：设置 → 应用 → 访问令牌；码云：设置 → 私人令牌"},
	}},
	{Type: "registry", Label: "镜像仓库", Description: "镜像仓库账号（仓库地址在 Docker 登录步骤里填），适用于任意 Docker Registry", Fields: []ConnField{
		{Key: "platform", Label: "平台", Options: []string{"acr", "dockerhub", "harbor", "ghcr", "other"}, Description: "登录都走 docker login，平台只用来区分展示"},
		{Key: "username", Label: "用户名", Required: true},
		{Key: "password", Label: "密码", Secret: true, Required: true},
	}},
	{Type: "npm", Label: "NPM 仓库", Description: "NPM 发包用的仓库账号（npmjs.com / Verdaccio 私服 / 阿里云制品仓库）：用户名 + 密码，或者访问令牌", Testable: true, Fields: []ConnField{
		{Key: "registry", Label: "仓库地址", Required: true, Placeholder: "https://registry.npmjs.org/"},
		{Key: "username", Label: "用户名"},
		{Key: "password", Label: "密码", Secret: true},
		{Key: "token", Label: "访问令牌", Secret: true, Description: "npmjs.com 只能用令牌发包，Verdaccio 用户名 + 密码即可"},
	}},
	{Type: "oss", Label: "对象存储OSS", Description: "S3 兼容的对象存储（阿里云 OSS / MinIO / Cloudflare R2 / AWS S3）", Testable: true, Fields: []ConnField{
		{Key: "platform", Label: "平台", Options: []string{"aliyun", "minio", "r2", "s3", "other"}, Description: "读写都走 S3 协议，平台只用来区分展示；MinIO 记得开路径风格"},
		{Key: "endpoint", Label: "Endpoint", Required: true, Placeholder: "https://oss-cn-hangzhou.aliyuncs.com"},
		{Key: "region", Label: "地域", Placeholder: "us-east-1"},
		{Key: "bucket", Label: "默认 Bucket"},
		{Key: "access_key_id", Label: "AccessKey ID", Required: true},
		{Key: "access_key_secret", Label: "AccessKey Secret", Secret: true, Required: true},
		{Key: "path_style", Label: "路径风格（MinIO 需开启）", Bool: true},
		{Key: "public_domain", Label: "公开访问域名", Description: "选填：默认 Bucket 绑定的自定义域 / CDN 域名；填了「OSS 上传」会输出 OSS_URL = 域名/对象路径"},
	}},
	{Type: "bark", Label: "Bark推送", Description: "iOS Bark 通知", Testable: true, Fields: []ConnField{
		{Key: "server", Label: "服务器", Placeholder: "https://api.day.app"},
		{Key: "device_key", Label: "Device Key", Secret: true, Required: true, Description: "Bark App 推送地址 /<Key>/ 里那一段；注意不是 64 位十六进制的 Device Token"},
	}},
	{Type: "wecom-bot", Label: "企业微信机器人", Description: "企业微信群机器人 webhook，「企业微信通知」插件和步骤用它往群里推运行结果", Testable: true, Fields: []ConnField{
		{Key: "webhook", Label: "Webhook 地址", Secret: true, Required: true, Placeholder: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=…", Description: "群聊 → 添加群机器人后复制的地址；key 就是发送权限，不要泄露"},
	}},
	{Type: "telegram", Label: "Telegram", Description: "Telegram 机器人：「Telegram 推送」步骤、「Telegram 通知」插件往会话里发消息，「Telegram 审批」发带审批链接的待审批消息", Testable: true, Fields: []ConnField{
		{Key: "bot_token", Label: "机器人令牌", Secret: true, Required: true, Placeholder: "123456789:AA…", Description: "@BotFather → /newbot 创建机器人后给的令牌"},
		{Key: "chat_id", Label: "会话 ID", Required: true, Description: "群 / 超级群是负数；私聊是自己的用户 id，要先给机器人发过一条消息"},
		{Key: "api_base", Label: "API 地址", Placeholder: "https://api.telegram.org", Description: "反代时填写，要原样转发 /bot<令牌>/<方法>"},
	}},
	{Type: "email", Label: "邮件", Description: "发邮件的账号：SMTP（企业邮箱 / QQ / Gmail 等），「邮件通知」步骤、插件和「邮件审批」都用它", Testable: true, Fields: []ConnField{
		{Key: "host", Label: "SMTP 服务器", Required: true, Placeholder: "smtp.qq.com"},
		{Key: "port", Label: "端口", Required: true, Placeholder: "465"},
		{Key: "tls", Label: "SSL/TLS 直连（465 端口）", Bool: true, Description: "关闭时先明文连接，服务器支持就升级为 STARTTLS（587 端口常见）"},
		{Key: "username", Label: "用户名"},
		{Key: "password", Label: "密码 / 授权码", Secret: true},
		{Key: "from", Label: "发件人", Description: "留空用用户名；带显示名的写法：显示名 <地址>"},
		{Key: "reply_to", Label: "回复地址"},
		{Key: "test_to", Label: "测试收件人", Description: "「测试发送」发到这个地址，留空发给发件人"},
	}},
	{Type: "cloudreve", Label: "Cloudreve 网盘", Description: "Cloudreve v4 网盘，用账号密码登录换取访问令牌（自动续期）", Testable: true, Fields: []ConnField{
		{Key: "server", Label: "站点地址", Required: true, Placeholder: "https://pan.example.com"},
		{Key: "email", Label: "登录邮箱", Required: true},
		{Key: "password", Label: "密码", Secret: true, Required: true},
	}},
}

func connType(t string) *ConnType {
	for i := range ConnTypes {
		if ConnTypes[i].Type == t {
			return &ConnTypes[i]
		}
	}
	return nil
}

func connAAD(tenantID, id, key string) string { return "conn/" + tenantID + "/" + id + "/" + key }

// ConnInput 是新建 / 修改服务连接的请求；私密字段留空或为 SecretMask 时保持原值。
type ConnInput struct {
	Type         string            `json:"type"`
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	Values       map[string]string `json:"values"`
	Visibility   string            `json:"visibility"`
	AllowedUsers []string          `json:"allowed_users"`
}

// connVisible 判断用户能否在流水线中引用该连接（管理员总能引用）。
func connVisible(c *model.ServiceConnection, username string, isAdmin bool) bool {
	return isAdmin || c.CreatedBy == username || c.Visibility == "public" ||
		(c.Visibility == "custom" && slices.Contains(c.AllowedUsers, username))
}

// Connections 返回用户能看到的连接（管理员看全部）。
func (e *Engine) Connections(ctx context.Context, tenantID, username string, isAdmin bool) ([]*model.ServiceConnection, error) {
	all, err := e.store.ListConnections(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, c := range all {
		if connVisible(c, username, isAdmin) {
			out = append(out, c)
		}
	}
	return out, nil
}

// SaveConnection 新建（id 为空）或修改服务连接；修改限创建人和管理员。
func (e *Engine) SaveConnection(ctx context.Context, tenantID, id, username string, isAdmin bool, in ConnInput) (*model.ServiceConnection, error) {
	in.Name, in.Description = strings.TrimSpace(in.Name), strings.TrimSpace(in.Description)
	if in.Name == "" || utf8.RuneCountInString(in.Name) > 64 || utf8.RuneCountInString(in.Description) > 200 {
		return nil, fmt.Errorf("%w: connection name is required (at most 64 characters)", ErrInvalid)
	}
	switch in.Visibility {
	case "":
		in.Visibility = "private"
	case "private", "public", "custom":
	default:
		return nil, fmt.Errorf("%w: visibility must be private, public or custom", ErrInvalid)
	}
	now := e.now()
	c := &model.ServiceConnection{ID: id, TenantID: tenantID, Type: in.Type, Name: in.Name, Description: in.Description,
		Config: map[string]string{}, Secrets: map[string]string{}, Visibility: in.Visibility, CreatedBy: username, CreatedAt: now, UpdatedAt: now}
	if in.Visibility == "custom" {
		c.AllowedUsers = slices.Compact(slices.Sorted(slices.Values(in.AllowedUsers)))
	}
	var old *model.ServiceConnection
	if id != "" {
		var err error
		if old, err = e.store.GetConnection(ctx, id); err != nil {
			return nil, err
		}
		if old.TenantID != tenantID {
			return nil, store.ErrNotFound
		}
		if !isAdmin && old.CreatedBy != username {
			return nil, fmt.Errorf("%w: only the creator or an admin can change this connection", ErrForbidden)
		}
		c.Type, c.CreatedBy, c.CreatedAt = old.Type, old.CreatedBy, old.CreatedAt
	} else {
		c.ID = "sc-" + randHex(8)
	}
	ct := connType(c.Type)
	if ct == nil {
		return nil, fmt.Errorf("%w: unknown connection type %q", ErrInvalid, c.Type)
	}
	for _, f := range ct.Fields {
		v := strings.TrimSpace(in.Values[f.Key])
		if f.Secret {
			if v == "" || v == SecretMask {
				if old != nil && old.Secrets[f.Key] != "" {
					c.Secrets[f.Key] = old.Secrets[f.Key]
				} else if f.Required {
					return nil, fmt.Errorf("%w: %s is required", ErrInvalid, f.Label)
				}
				continue
			}
			c.Secrets[f.Key] = e.sealString(v, connAAD(tenantID, c.ID, f.Key))
			continue
		}
		if f.Required && v == "" {
			return nil, fmt.Errorf("%w: %s is required", ErrInvalid, f.Label)
		}
		if len(f.Options) > 0 && v != "" && !slices.Contains(f.Options, v) {
			return nil, fmt.Errorf("%w: %s must be one of %s", ErrInvalid, f.Label, strings.Join(f.Options, ", "))
		}
		if f.Bool && v != "" && v != "true" && v != "false" {
			return nil, fmt.Errorf("%w: %s must be true or false", ErrInvalid, f.Label)
		}
		if v != "" {
			c.Config[f.Key] = v
		}
	}
	if c.Type == "git" && c.Config["platform"] == "gitea" && c.Config["server"] == "" {
		return nil, fmt.Errorf("%w: Gitea needs the site address", ErrInvalid)
	}
	if c.Type == "npm" && c.Secrets["token"] == "" && (c.Config["username"] == "" || c.Secrets["password"] == "") {
		return nil, fmt.Errorf("%w: an access token, or username and password, is required", ErrInvalid)
	}
	if err := e.store.PutConnection(ctx, c); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, fmt.Errorf("%w: a connection named %q already exists", store.ErrConflict, c.Name)
		}
		return nil, err
	}
	return e.store.GetConnection(ctx, c.ID)
}

func (e *Engine) DeleteConnection(ctx context.Context, tenantID, id, username string, isAdmin bool) error {
	c, err := e.store.GetConnection(ctx, id)
	if err != nil {
		return err
	}
	if c.TenantID != tenantID {
		return store.ErrNotFound
	}
	if !isAdmin && c.CreatedBy != username {
		return fmt.Errorf("%w: only the creator or an admin can delete this connection", ErrForbidden)
	}
	return e.store.DeleteConnection(ctx, id)
}

// connValues 解密连接的全部字段（非私密 + 私密）。
func (e *Engine) connValues(c *model.ServiceConnection) (map[string]string, error) {
	out := maps.Clone(c.Config)
	if out == nil {
		out = map[string]string{}
	}
	for k, v := range c.Secrets {
		plain, err := e.openString(v, connAAD(c.TenantID, c.ID, k))
		if err != nil {
			return nil, fmt.Errorf("decrypt %s.%s: %w", c.Name, k, err)
		}
		out[k] = plain
	}
	return out, nil
}

// ConnEnv 把连接字段转成注入步骤的环境变量 CONN_<KEY>；返回的 secret 为需要在日志中脱敏的值。
func ConnEnv(values map[string]string, ct *ConnType) (env map[string]string, secret []string) {
	env = map[string]string{"CONN_TYPE": ct.Type}
	for _, f := range ct.Fields {
		v, ok := values[f.Key]
		if !ok {
			if f.Bool {
				v = "false"
			} else {
				continue
			}
		}
		env["CONN_"+strings.ToUpper(f.Key)] = v
		if f.Secret && v != "" {
			secret = append(secret, v)
		}
	}
	return env, secret
}

// connRefs 返回流水线引用的服务连接及要求的类型（内置步骤、插件、审批消息）。
func connRefs(spec *pipeline.Spec) []pipeline.ConnRef {
	var out []pipeline.ConnRef
	for _, st := range spec.Stages {
		for _, j := range st.Jobs {
			for i, step := range j.Steps {
				if step.Connection == "" {
					continue
				}
				ref := pipeline.ConnRef{Name: step.Connection, Where: fmt.Sprintf("job %q step %d (%s)", j.Name, i+1, step.Name)}
				if k := steps.Get(step.Kind); k != nil {
					ref.Type = k.Connection
				}
				out = append(out, ref)
			}
			for i, p := range j.Plugins {
				if p.Connection != "" {
					out = append(out, pipeline.ConnRef{Name: p.Connection, Type: pipeline.PluginTypes[p.Type], Where: fmt.Sprintf("job %q plugin %d", j.Name, i+1)})
				}
			}
			for i, n := range j.Notify {
				out = append(out, pipeline.ConnRef{Name: n.Connection, Type: "message", Where: fmt.Sprintf("job %q notify %d", j.Name, i+1)})
			}
		}
	}
	return out
}

// checkConnections 校验流水线引用的服务连接存在、类型匹配且保存者可以引用（user 为空时不校验可见性）。
func (e *Engine) checkConnections(ctx context.Context, tenantID string, spec *pipeline.Spec, user string, isAdmin bool) error {
	for _, ref := range connRefs(spec) {
		c, err := e.store.GetConnectionByName(ctx, tenantID, ref.Name)
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: %s uses service connection %q, which does not exist", ErrInvalid, ref.Where, ref.Name)
		}
		if err != nil {
			return err
		}
		switch {
		case ref.Type == "message" && !connect.Sendable(c.Type):
			return fmt.Errorf("%w: %s: %q cannot send messages (use bark, wecom-bot, telegram or email)", ErrInvalid, ref.Where, ref.Name)
		case ref.Type != "" && ref.Type != "message" && c.Type != ref.Type:
			return fmt.Errorf("%w: %s needs a %s connection, %q is %s", ErrInvalid, ref.Where, ref.Type, ref.Name, c.Type)
		}
		if user != "" && !connVisible(c, user, isAdmin) {
			return fmt.Errorf("%w: you cannot use service connection %q", ErrInvalid, ref.Name)
		}
	}
	return nil
}

// checkKinds 校验内置步骤：类型存在、输入合法、需要服务连接的已选择。
func checkKinds(spec *pipeline.Spec) error {
	for _, st := range spec.Stages {
		for _, j := range st.Jobs {
			for i, step := range j.Steps {
				if step.Kind == "" {
					continue
				}
				k := steps.Get(step.Kind)
				if k == nil {
					return fmt.Errorf("%w: job %q step %d: unknown step kind %q", ErrInvalid, j.Name, i+1, step.Kind)
				}
				if _, _, err := k.Expand(step.With); err != nil {
					return fmt.Errorf("%w: job %q step %d: %v", ErrInvalid, j.Name, i+1, err)
				}
				if k.Connection != "" && step.Connection == "" {
					return fmt.Errorf("%w: job %q step %d (%s) needs a %s service connection", ErrInvalid, j.Name, i+1, k.Label, k.Connection)
				}
			}
		}
	}
	return nil
}

// expandKind 把内置步骤展开为模型中的步骤：脚本 + INPUT_* 变量（步骤自己的 env 优先）。
func expandKind(s pipeline.Step) (model.Step, error) {
	out := model.Step{Name: s.Name, Run: s.Run, Env: s.Env, Connection: s.Connection}
	if s.Kind == "" {
		return out, nil
	}
	k := steps.Get(s.Kind)
	if k == nil {
		return out, fmt.Errorf("%w: unknown step kind %q", ErrInvalid, s.Kind)
	}
	script, env, err := k.Expand(s.With)
	if err != nil {
		return out, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	maps.Copy(env, s.Env)
	out.Run, out.Env, out.Kind, out.With = script, env, s.Kind, maps.Clone(s.With)
	if out.Name == s.Kind {
		out.Name = k.Label
	}
	return out, nil
}

// isTenantAdmin 判断用户是否为租户管理员（或系统管理员）。
func (e *Engine) isTenantAdmin(ctx context.Context, tenantID, username string) bool {
	u, err := e.store.GetUserByName(ctx, username)
	if err != nil {
		return false
	}
	if u.IsAdmin {
		return true
	}
	ms, err := e.store.UserMemberships(ctx, u.ID)
	return err == nil && slices.ContainsFunc(ms, func(m *model.Membership) bool { return m.TenantID == tenantID && m.Role == model.RoleAdmin })
}

// checkSaveConnections 是保存流水线时的服务连接校验：保存者必须能引用所用的连接。
func (e *Engine) checkSaveConnections(ctx context.Context, tenantID string, spec *pipeline.Spec, user string) error {
	if len(connRefs(spec)) == 0 {
		return nil
	}
	return e.checkConnections(ctx, tenantID, spec, user, user == "" || e.isTenantAdmin(ctx, tenantID, user))
}

// TestConnection 用表单中的值测试连接（私密字段留空或为掩码时用已保存的值）。
func (e *Engine) TestConnection(ctx context.Context, tenantID, id, username string, isAdmin bool, in ConnInput) error {
	vals := map[string]string{}
	typ := in.Type
	if id != "" {
		c, err := e.store.GetConnection(ctx, id)
		if err != nil {
			return err
		}
		if c.TenantID != tenantID || !connVisible(c, username, isAdmin) {
			return store.ErrNotFound
		}
		if vals, err = e.connValues(c); err != nil {
			return err
		}
		typ = c.Type
	}
	ct := connType(typ)
	if ct == nil {
		return fmt.Errorf("%w: unknown connection type %q", ErrInvalid, typ)
	}
	for _, f := range ct.Fields {
		v, ok := in.Values[f.Key]
		v = strings.TrimSpace(v)
		if !ok || (f.Secret && (v == "" || v == SecretMask)) {
			continue
		}
		vals[f.Key] = v
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := connect.Test(ctx, typ, vals); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return nil
}

// SetRunRemark 修改运行备注。
func (e *Engine) SetRunRemark(ctx context.Context, runID, remark string) error {
	remark = strings.TrimSpace(remark)
	if utf8.RuneCountInString(remark) > maxRemarkLen {
		return fmt.Errorf("%w: remark must be at most %d characters", ErrInvalid, maxRemarkLen)
	}
	if err := e.store.SetRunRemark(ctx, runID, remark); err != nil {
		return err
	}
	e.runChanged(runID)
	return nil
}

// resolveConnections 解析 Job 步骤引用的服务连接，写入 a.Connections / a.Masks；返回无法解析时的说明。
func (e *Engine) resolveConnections(ctx context.Context, j *model.Job, a *Assignment) string {
	var problems []string
	for _, st := range j.Steps {
		name := st.Connection
		if name == "" || a.Connections[name] != nil {
			continue
		}
		c, err := e.store.GetConnectionByName(ctx, j.TenantID, name)
		if err != nil {
			problems = append(problems, fmt.Sprintf("service connection %q: %v", name, err))
			continue
		}
		vals, err := e.connValues(c)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		env, secrets := ConnEnv(vals, connType(c.Type))
		if a.Connections == nil {
			a.Connections = map[string]map[string]string{}
		}
		a.Connections[name] = env
		a.Masks = append(a.Masks, secrets...)
	}
	return strings.Join(problems, "; ")
}
