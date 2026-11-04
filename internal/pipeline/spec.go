// Package pipeline 解析并校验 YAML 流水线定义。
//
// 语义：Stage 之间默认串行，Stage 内的 Job 并行；
// Job 未声明 needs 时依赖上一个 Stage 的全部 Job，
// 声明 needs 时只依赖所列 Job（可提前启动，组成 DAG），needs: [] 表示无依赖。
// 带 matrix 的 Job 在解析时展开为多个 Job，needs 引用其原名时依赖全部展开项。
package pipeline

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"flow/internal/gitstatus"
)

const (
	JobTypeApproval = "approval" // 人工卡点：依赖满足后等待审批，不需要 Runner

	WhenOnSuccess = "on_success" // 默认：上游全部成功
	WhenOnFailure = "on_failure" // 上游结束且本次运行已有 Job 失败
	WhenAlways    = "always"     // 上游结束即执行（不论成败）

	maxMatrixJobs = 64
	maxRetry      = 5
)

type Spec struct {
	Name string            `yaml:"name"`
	Env  map[string]string `yaml:"env"`
	// Vars 是带说明的变量（与 Env 一样注入每个任务，解析后合并进 Env）；Runtime 为 true 的可在运行时
	// （运行弹窗、流水线 Webhook）覆盖默认值。
	Vars []Var `yaml:"vars"`
	// VarGroups 是关联的通用变量组名称；为空时注入租户的全部变量组。
	VarGroups []string `yaml:"var_groups"`
	// CacheDirs 是缓存目录：容器内的绝对路径，挂载为 Runner 上按流水线隔离的持久目录，下次运行直接复用。
	CacheDirs []string   `yaml:"cache_dirs"`
	Params    []Param    `yaml:"params"`
	Source    *Source    `yaml:"source"`
	Notify    []Notify   `yaml:"notify"`
	Retention *Retention `yaml:"retention"`
	// Concurrency 限制同一并发组同时只有一个运行在执行。
	Concurrency *Concurrency `yaml:"concurrency"`
	Stages      []Stage      `yaml:"stages"`
}

// Concurrency 是运行的并发控制：同组运行依次执行，后来的排队（或取消之前的）。
type Concurrency struct {
	// Group 为并发组名，可含 ${变量}（运行参数、env），如 deploy-${TARGET}；租户内同名即同组，可跨流水线。
	// 省略表示只与本流水线的其他运行互斥。
	Group string `yaml:"group"`
	// CancelPrevious 为 true 时，新运行取消同组中尚未结束的运行（适合只关心最新提交的场景）。
	CancelPrevious bool `yaml:"cancel_previous"`
	// Max 是同组同时执行的运行数上限（默认 1），超出的按触发顺序排队。
	Max int `yaml:"max"`
}

// Var 是流水线变量。
type Var struct {
	Name        string `yaml:"name"`
	Value       string `yaml:"value"`
	Description string `yaml:"description"`
	Runtime     bool   `yaml:"runtime"`
}

const (
	maxConcurrency = 20
	maxVars        = 200
)

// RuntimeNames 返回运行时可以覆盖的变量名：运行参数与开启了运行时设置的变量。
func (s *Spec) RuntimeNames() map[string]bool {
	out := map[string]bool{}
	for _, p := range s.Params {
		out[p.Name] = true
	}
	for _, v := range s.Vars {
		if v.Runtime {
			out[v.Name] = true
		}
	}
	return out
}

func (s *Spec) validateVars() error {
	if len(s.Vars) > maxVars {
		return fmt.Errorf("at most %d vars", maxVars)
	}
	seen := map[string]bool{}
	for i, v := range s.Vars {
		if !ValidEnvName(v.Name) {
			return fmt.Errorf("vars[%d]: %q is not a valid variable name", i, v.Name)
		}
		if seen[v.Name] {
			return fmt.Errorf("duplicate var %q", v.Name)
		}
		if _, ok := s.Env[v.Name]; ok {
			return fmt.Errorf("var %q is also defined in env", v.Name)
		}
		seen[v.Name] = true
	}
	for _, v := range s.Vars {
		if s.Env == nil {
			s.Env = map[string]string{}
		}
		s.Env[v.Name] = v.Value
	}
	groups := map[string]bool{}
	for _, g := range s.VarGroups {
		if strings.TrimSpace(g) == "" || groups[g] {
			return fmt.Errorf("var_groups: empty or duplicate group %q", g)
		}
		groups[g] = true
	}
	return nil
}

const maxGroupLen = 100

// ConcurrencyGroup 返回运行所属的并发组（未配置并发控制时为空）；pipelineID 用于默认组名。
func (s *Spec) ConcurrencyGroup(pipelineID string, env map[string]string) string {
	c := s.Concurrency
	if c == nil {
		return ""
	}
	if strings.TrimSpace(c.Group) == "" {
		return "pipeline/" + pipelineID
	}
	vars := mergeVars(s.Env, env)
	return strings.TrimSpace(groupVar.ReplaceAllStringFunc(c.Group, func(ref string) string { return vars[ref[2:len(ref)-1]] }))
}

var groupVar = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

func mergeVars(layers ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, l := range layers {
		for k, v := range l {
			out[k] = v
		}
	}
	return out
}

// Retention 是流水线的保留策略。
type Retention struct {
	Artifacts string `yaml:"artifacts"` // 产物保留时长，如 7d、36h；0 为永久；省略用 Server 默认值
	Runs      int    `yaml:"runs"`      // 最多保留的运行数（已结束的旧运行连同日志、产物一起删除）；省略为不限

	// ArtifactTTL 为解析后的产物保留时长：0 表示用 Server 默认值，ForeverTTL 表示永久。
	ArtifactTTL time.Duration `yaml:"-"`
}

// ForeverTTL 表示永久保留。
const ForeverTTL time.Duration = -1

const maxRetainRuns = 100000

// ParseRetention 解析保留时长：Go duration（36h、90m）或天数（7d）；0 表示永久，返回 ForeverTTL。
func ParseRetention(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "0" {
		return ForeverTTL, nil
	}
	if days, ok := strings.CutSuffix(v, "d"); ok {
		var n int
		if _, err := fmt.Sscanf(days, "%d", &n); err == nil && n > 0 && fmt.Sprint(n) == days {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	} else if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d, nil
	}
	return 0, fmt.Errorf("invalid retention %q (examples: 7d, 36h, 0 for forever)", v)
}

// Source 是代码源：配置后 Runner 在执行 Step 前自动拉取代码到工作目录。
type Source struct {
	URL         string `yaml:"url"`
	Branch      string `yaml:"branch"`       // 默认分支；Webhook / 手动参数 FLOW_GIT_BRANCH 可覆盖
	Username    string `yaml:"username"`     // HTTP 认证用户名，默认 oauth2
	TokenSecret string `yaml:"token_secret"` // 存放访问令牌的密钥名
	Depth       int    `yaml:"depth"`        // 浅克隆深度，默认 50
	// ReportStatus 为 true 时把运行结果回写为提交状态（需 token_secret 有写状态权限）。
	ReportStatus bool   `yaml:"report_status"`
	Provider     string `yaml:"provider"` // github / gitlab / gitea，省略时按仓库地址推断
}

// Notify 是运行结束时的通知渠道；url / secret 中的 ${NAME} 会替换为同名密钥。
type Notify struct {
	Type   string   `yaml:"type"` // dingtalk / feishu / wecom / webhook
	URL    string   `yaml:"url"`
	Secret string   `yaml:"secret"` // 钉钉 / 飞书加签密钥
	On     []string `yaml:"on"`     // success / failed / canceled，默认 [failed]
}

type Stage struct {
	Name string `yaml:"name"`
	Jobs []Job  `yaml:"jobs"`
}

type Job struct {
	Name      string              `yaml:"name"`
	Type      string              `yaml:"type"`  // 空为普通 Job；approval 为人工卡点
	Image     string              `yaml:"image"` // 为空时直接在 Runner 主机上执行
	RunsOn    StringList          `yaml:"runs-on"`
	Needs     []string            `yaml:"needs"`
	When      string              `yaml:"when"`
	Env       map[string]string   `yaml:"env"`
	Matrix    map[string][]string `yaml:"matrix"`
	Timeout   string              `yaml:"timeout"` // Go duration，如 30m
	Retry     int                 `yaml:"retry"`   // 失败后自动重试次数
	Checkout  *bool               `yaml:"checkout"`
	Artifacts []string            `yaml:"artifacts"` // 成功后上传的路径（相对工作目录，支持通配符）
	Cache     *Cache              `yaml:"cache"`
	Approvers []string            `yaml:"approvers"` // 人工卡点的审批人用户名，空为任意有权限的成员
	Deploy    *Deploy             `yaml:"deploy"`    // 主机部署：步骤通过 SSH 在主机组的每台主机上执行
	Reports   *Reports            `yaml:"reports"`   // 测试报告：步骤结束后（无论成败）解析并上传
	// If 是运行条件：上游结束后求值，为假时跳过（见 ParseCond）。
	If string `yaml:"if"`
	// Plugins 是任务插件：任务结束时按状态发送通知。
	Plugins []Plugin `yaml:"plugins"`
	// Notify 是人工卡点进入待审批时发送审批消息（带通过 / 驳回链接）的服务连接。
	Notify []ApprovalNotify `yaml:"notify"`
	Steps  []Step           `yaml:"steps"`

	TimeoutDuration time.Duration `yaml:"-"`
	MatrixValues    []string      `yaml:"-"` // 展开后的取值（KEY=VALUE），用于展示
}

// Deploy 是主机部署任务的目标：先把 upload 上传到每台主机的 target 目录，再在该目录执行步骤。
type Deploy struct {
	Hosts  string `yaml:"hosts"`  // 主机组名（在「环境与主机」中维护）
	Upload string `yaml:"upload"` // 可选：上传工作目录中的文件或目录（如上游产物 dist）
	Target string `yaml:"target"` // 远程目录，默认 /tmp/flow-deploy/<流水线名>
	Batch  int    `yaml:"batch"`  // 每批主机数，默认 1（逐台滚动）；前一批全部成功才继续
}

const maxDeployBatch = 1000

// Reports 是任务产生的测试报告。
type Reports struct {
	JUnit []string `yaml:"junit"` // JUnit XML 路径，相对工作目录，支持通配符
}

// Cache 是 Runner 本地缓存：按 key 在执行前恢复、成功后保存 paths。
type Cache struct {
	Key   string   `yaml:"key"`
	Paths []string `yaml:"paths"`
}

// Step 是一条 Shell 命令，或以 uses 引用租户的步骤模板（创建运行时展开为模板中的步骤）。
type Step struct {
	Name string            `yaml:"name"`
	Run  string            `yaml:"run"`
	Env  map[string]string `yaml:"env"`  // 仅本步骤可见；值中的 ${NAME} 在执行时按 Job 变量与密钥展开
	Uses string            `yaml:"uses"` // 步骤模板名，可用 name@版本 锁定版本（如 go-build@3），否则用最新版本
	With map[string]string `yaml:"with"` // 模板输入，注入为模板步骤的环境变量
	// Connection 是引用的服务连接名称：执行时其字段注入为 CONN_<字段>（私密字段在日志中脱敏）。
	Connection string `yaml:"connection"`
	// Kind 是内置步骤类型（见「添加步骤」中的步骤库），With 为其输入；创建运行时展开为脚本。
	Kind string `yaml:"kind"`
}

// StringList 同时接受单个字符串与字符串列表。
type StringList []string

func (l *StringList) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*l = StringList{n.Value}
		return nil
	}
	var ss []string
	if err := n.Decode(&ss); err != nil {
		return err
	}
	*l = ss
	return nil
}

// IsApproval 表示该 Job 是人工卡点。
func (j *Job) IsApproval() bool { return j.Type == JobTypeApproval }

// Parse 解析 YAML，补全默认值、展开矩阵并校验。
func Parse(data []byte) (*Spec, error) {
	var s Spec
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	if err := s.validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// Dependencies 返回每个 Job 实际依赖的上游 Job 名。
func (s *Spec) Dependencies() map[string][]string {
	deps := make(map[string][]string)
	var prev []string
	for _, st := range s.Stages {
		var names []string
		for _, j := range st.Jobs {
			if j.Needs != nil {
				deps[j.Name] = slices.Clone(j.Needs)
			} else {
				deps[j.Name] = slices.Clone(prev)
			}
			names = append(names, j.Name)
		}
		prev = names
	}
	return deps
}

func (s *Spec) validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return errors.New("name is required")
	}
	if len(s.Stages) == 0 {
		return errors.New("at least one stage is required")
	}
	if err := s.validateSource(); err != nil {
		return err
	}
	if err := s.validateNotify(); err != nil {
		return err
	}
	if err := s.validateParams(); err != nil {
		return err
	}
	if err := s.validateVars(); err != nil {
		return err
	}
	for _, d := range s.CacheDirs {
		if !strings.HasPrefix(d, "/") || strings.Contains(d, "..") || strings.ContainsAny(d, ":,") {
			return fmt.Errorf("cache_dirs: %q must be an absolute path", d)
		}
	}
	if err := s.validateConcurrency(); err != nil {
		return err
	}
	if r := s.Retention; r != nil {
		if r.Artifacts != "" {
			d, err := ParseRetention(r.Artifacts)
			if err != nil {
				return fmt.Errorf("retention.artifacts: %w", err)
			}
			r.ArtifactTTL = d
		}
		if r.Runs < 0 || r.Runs > maxRetainRuns {
			return fmt.Errorf("retention.runs must be between 0 and %d", maxRetainRuns)
		}
	}

	stageNames := make(map[string]bool)
	expansions := make(map[string][]string) // 矩阵 Job 原名 -> 展开后的名字
	for si := range s.Stages {
		st := &s.Stages[si]
		if st.Name == "" {
			return fmt.Errorf("stages[%d]: name is required", si)
		}
		if stageNames[st.Name] {
			return fmt.Errorf("duplicate stage %q", st.Name)
		}
		stageNames[st.Name] = true
		if len(st.Jobs) == 0 {
			return fmt.Errorf("stage %q: at least one job is required", st.Name)
		}
		var jobs []Job
		for ji := range st.Jobs {
			j := &st.Jobs[ji]
			if j.Name == "" {
				return fmt.Errorf("stage %q jobs[%d]: name is required", st.Name, ji)
			}
			if err := j.validate(); err != nil {
				return fmt.Errorf("job %q: %w", j.Name, err)
			}
			expanded, err := j.expandMatrix()
			if err != nil {
				return fmt.Errorf("job %q: %w", j.Name, err)
			}
			if len(j.Matrix) > 0 {
				for _, e := range expanded {
					expansions[j.Name] = append(expansions[j.Name], e.Name)
				}
			}
			jobs = append(jobs, expanded...)
		}
		st.Jobs = jobs
	}

	stageOf := make(map[string]int)
	for si, st := range s.Stages {
		for _, j := range st.Jobs {
			if _, dup := stageOf[j.Name]; dup {
				return fmt.Errorf("duplicate job %q", j.Name)
			}
			stageOf[j.Name] = si
		}
	}
	for si := range s.Stages {
		for ji := range s.Stages[si].Jobs {
			j := &s.Stages[si].Jobs[ji]
			if j.Needs == nil {
				continue
			}
			needs := []string{}
			for _, n := range j.Needs {
				if names, ok := expansions[n]; ok {
					needs = append(needs, names...)
				} else {
					needs = append(needs, n)
				}
			}
			j.Needs = needs
			for _, n := range needs {
				ds, ok := stageOf[n]
				switch {
				case !ok:
					return fmt.Errorf("job %q needs unknown job %q", j.Name, n)
				case n == j.Name:
					return fmt.Errorf("job %q needs itself", j.Name)
				case ds > si:
					return fmt.Errorf("job %q needs %q from a later stage", j.Name, n)
				}
			}
		}
	}
	return s.checkCycles()
}

func (s *Spec) validateSource() error {
	src := s.Source
	if src == nil {
		return nil
	}
	if strings.TrimSpace(src.URL) == "" {
		return errors.New("source.url is required")
	}
	if src.Depth < 0 {
		return errors.New("source.depth must not be negative")
	}
	if src.TokenSecret != "" && !ValidEnvName(src.TokenSecret) {
		return fmt.Errorf("source.token_secret %q is not a valid secret name", src.TokenSecret)
	}
	if src.Provider != "" && !slices.Contains(gitstatus.Providers, src.Provider) {
		return fmt.Errorf("source.provider must be one of %s", strings.Join(gitstatus.Providers, ", "))
	}
	if src.ReportStatus {
		if src.TokenSecret == "" {
			return errors.New("source.report_status needs source.token_secret (a token allowed to write commit statuses)")
		}
		if _, err := gitstatus.Parse(src.URL, src.Provider); err != nil {
			return fmt.Errorf("source.report_status: %w", err)
		}
	}
	return nil
}

func (s *Spec) validateConcurrency() error {
	c := s.Concurrency
	if c == nil {
		return nil
	}
	if len(c.Group) > maxGroupLen {
		return fmt.Errorf("concurrency.group must be at most %d characters", maxGroupLen)
	}
	if c.Max < 0 || c.Max > maxConcurrency {
		return fmt.Errorf("concurrency.max must be between 1 and %d", maxConcurrency)
	}
	if c.Max > 1 && c.CancelPrevious {
		return errors.New("concurrency.cancel_previous cannot be combined with max > 1")
	}
	for _, m := range groupVar.FindAllStringSubmatch(c.Group, -1) {
		if _, ok := s.Env[m[1]]; ok {
			continue
		}
		if !slices.ContainsFunc(s.Params, func(p Param) bool { return p.Name == m[1] }) && !strings.HasPrefix(m[1], "FLOW_") {
			return fmt.Errorf("concurrency.group uses ${%s}, which is neither a param nor an env variable", m[1])
		}
	}
	return nil
}

var notifyTypes = []string{"dingtalk", "feishu", "wecom", "webhook"}
var notifyEvents = []string{"success", "failed", "canceled"}

func (s *Spec) validateNotify() error {
	for i := range s.Notify {
		n := &s.Notify[i]
		if !slices.Contains(notifyTypes, n.Type) {
			return fmt.Errorf("notify[%d]: type must be one of %s", i, strings.Join(notifyTypes, ", "))
		}
		if strings.TrimSpace(n.URL) == "" {
			return fmt.Errorf("notify[%d]: url is required", i)
		}
		if len(n.On) == 0 {
			n.On = []string{"failed"}
		}
		for _, on := range n.On {
			if !slices.Contains(notifyEvents, on) {
				return fmt.Errorf("notify[%d]: on must be within %s", i, strings.Join(notifyEvents, ", "))
			}
		}
	}
	return nil
}

func (j *Job) validate() error {
	if err := j.validatePlugins(); err != nil {
		return err
	}
	switch j.Type {
	case "":
		if len(j.Steps) == 0 {
			return errors.New("at least one step is required")
		}
	case JobTypeApproval:
		if len(j.Steps) > 0 || j.Image != "" || len(j.Artifacts) > 0 || j.Cache != nil || len(j.Matrix) > 0 {
			return errors.New("approval job must not define steps, image, artifacts, cache or matrix")
		}
	default:
		return fmt.Errorf("unknown type %q (want approval or omit)", j.Type)
	}
	for k := range j.Steps {
		if err := j.Steps[k].validate(k, true); err != nil {
			return err
		}
	}
	switch j.When {
	case "":
		j.When = WhenOnSuccess
	case WhenOnSuccess, WhenOnFailure, WhenAlways:
	default:
		return fmt.Errorf("when must be on_success, on_failure or always, got %q", j.When)
	}
	if j.Timeout != "" {
		d, err := time.ParseDuration(j.Timeout)
		if err != nil || d <= 0 {
			return fmt.Errorf("invalid timeout %q (examples: 30m, 1h30m)", j.Timeout)
		}
		j.TimeoutDuration = d
	}
	if j.Retry < 0 || j.Retry > maxRetry {
		return fmt.Errorf("retry must be between 0 and %d", maxRetry)
	}
	if j.Retry > 0 && j.IsApproval() {
		return errors.New("approval job cannot retry")
	}
	if r := j.Reports; r != nil {
		if j.IsApproval() || j.Deploy != nil {
			return errors.New("reports are only supported on jobs that run commands on the runner")
		}
		for _, p := range r.JUnit {
			if err := checkRelPath(p); err != nil {
				return fmt.Errorf("reports.junit: %w", err)
			}
		}
	}
	if d := j.Deploy; d != nil {
		switch {
		case j.IsApproval():
			return errors.New("approval job cannot deploy")
		case strings.TrimSpace(d.Hosts) == "":
			return errors.New("deploy.hosts (host group name) is required")
		case j.Image != "":
			return errors.New("deploy job runs its steps on the target hosts and cannot set image")
		case d.Batch < 0 || d.Batch > maxDeployBatch:
			return fmt.Errorf("deploy.batch must be between 1 and %d", maxDeployBatch)
		case strings.ContainsAny(d.Target, "\n\r'"):
			return errors.New("deploy.target must not contain quotes or newlines")
		}
		if d.Upload != "" {
			if err := checkRelPath(d.Upload); err != nil {
				return fmt.Errorf("deploy.upload: %w", err)
			}
		}
		if d.Batch == 0 {
			d.Batch = 1
		}
	}
	for _, l := range j.RunsOn {
		if strings.TrimSpace(l) == "" {
			return errors.New("runs-on contains an empty label")
		}
	}
	for _, p := range j.Artifacts {
		if err := checkRelPath(p); err != nil {
			return fmt.Errorf("artifacts: %w", err)
		}
	}
	if c := j.Cache; c != nil {
		if strings.TrimSpace(c.Key) == "" || len(c.Paths) == 0 {
			return errors.New("cache requires key and paths")
		}
		for _, p := range c.Paths {
			if err := checkRelPath(p); err != nil {
				return fmt.Errorf("cache: %w", err)
			}
		}
	}
	return nil
}

func (step *Step) validate(k int, allowUses bool) error {
	hasRun := strings.TrimSpace(step.Run) != ""
	switch {
	case step.Kind != "" && (hasRun || step.Uses != ""):
		return fmt.Errorf("steps[%d]: kind cannot be combined with run or uses", k)
	case step.Kind != "":
		// 输入由引擎按步骤库校验
	case step.Uses != "" && !allowUses:
		return fmt.Errorf("steps[%d]: uses is not allowed here (templates cannot reference other templates)", k)
	case step.Uses != "" && hasRun:
		return fmt.Errorf("steps[%d]: run and uses are mutually exclusive", k)
	case step.Uses != "":
		if _, _, ok := ParseUses(step.Uses); !ok {
			return fmt.Errorf("steps[%d]: invalid template reference %q (want name or name@version, e.g. go-build@3)", k, step.Uses)
		}
	case !hasRun:
		return fmt.Errorf("steps[%d]: run or uses is required", k)
	case len(step.With) > 0:
		return fmt.Errorf("steps[%d]: with is only allowed together with uses", k)
	}
	for _, m := range []map[string]string{step.Env, step.With} {
		for name := range m {
			if !ValidEnvName(name) {
				return fmt.Errorf("steps[%d]: invalid variable name %q", k, name)
			}
		}
	}
	if step.Name == "" && step.Kind != "" {
		step.Name = step.Kind
	}
	if step.Name == "" {
		step.Name, _, _ = ParseUses(step.Uses)
		if step.Name == "" {
			step.Name = fmt.Sprintf("step-%d", k+1)
		}
	}
	return nil
}

// checkRelPath 拒绝绝对路径与 ..，保证只能读写 Job 工作目录内的文件。
func checkRelPath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || slices.Contains(strings.Split(p, "/"), "..") {
		return fmt.Errorf("path %q must be relative to the workspace and must not contain ..", p)
	}
	return nil
}

// expandMatrix 按键名排序做笛卡尔积，每个组合生成一个 Job，取值注入为同名环境变量。
func (j *Job) expandMatrix() ([]Job, error) {
	if len(j.Matrix) == 0 {
		return []Job{*j}, nil
	}
	keys := make([]string, 0, len(j.Matrix))
	total := 1
	for k, vals := range j.Matrix {
		if !ValidEnvName(k) {
			return nil, fmt.Errorf("matrix key %q is not a valid variable name", k)
		}
		if len(vals) == 0 {
			return nil, fmt.Errorf("matrix %q has no values", k)
		}
		keys = append(keys, k)
		total *= len(vals)
		if total > maxMatrixJobs {
			return nil, fmt.Errorf("matrix expands to more than %d jobs", maxMatrixJobs)
		}
	}
	sort.Strings(keys)
	combos := [][]string{{}}
	for _, k := range keys {
		var next [][]string
		for _, c := range combos {
			for _, v := range j.Matrix[k] {
				next = append(next, append(slices.Clone(c), v))
			}
		}
		combos = next
	}
	out := make([]Job, 0, len(combos))
	for _, c := range combos {
		e := *j
		e.Matrix = nil
		e.Env = make(map[string]string, len(j.Env)+len(keys))
		for k, v := range j.Env {
			e.Env[k] = v
		}
		e.MatrixValues = nil
		for i, k := range keys {
			e.Env[k] = c[i]
			e.MatrixValues = append(e.MatrixValues, k+"="+c[i])
		}
		e.Name = fmt.Sprintf("%s (%s)", j.Name, strings.Join(c, ", "))
		e.Steps = slices.Clone(j.Steps)
		out = append(out, e)
	}
	return out, nil
}

func (s *Spec) checkCycles() error {
	const (
		visiting = 1
		done     = 2
	)
	deps := s.Dependencies()
	state := make(map[string]int)
	var visit func(n string, path []string) error
	visit = func(n string, path []string) error {
		switch state[n] {
		case visiting:
			return fmt.Errorf("dependency cycle: %s", strings.Join(append(path, n), " -> "))
		case done:
			return nil
		}
		state[n] = visiting
		for _, d := range deps[n] {
			if err := visit(d, append(path, n)); err != nil {
				return err
			}
		}
		state[n] = done
		return nil
	}
	for _, st := range s.Stages {
		for _, j := range st.Jobs {
			if err := visit(j.Name, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

// ValidEnvName 判断是否为合法的环境变量 / 密钥名。
func ValidEnvName(k string) bool {
	if k == "" {
		return false
	}
	for i, c := range k {
		if c != '_' && !(c >= 'A' && c <= 'Z') && !(c >= 'a' && c <= 'z') && !(i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// MaxConcurrent 返回并发组允许同时执行的运行数（未配置为 0，表示不限）。
func (s *Spec) MaxConcurrent() int {
	if s.Concurrency == nil {
		return 0
	}
	return max(s.Concurrency.Max, 1)
}

// ConnRef 是流水线对服务连接的一处引用。
type ConnRef struct {
	Name  string
	Type  string // 要求的连接类型，空为不限
	Where string // 引用位置，用于报错
}

// ConnectionRefs 返回步骤引用的服务连接。
func (s *Spec) ConnectionRefs() []ConnRef {
	var out []ConnRef
	for _, st := range s.Stages {
		for _, j := range st.Jobs {
			for i, step := range j.Steps {
				if step.Connection != "" {
					name := step.Name
					if name == "" {
						name = fmt.Sprintf("step %d", i+1)
					}
					out = append(out, ConnRef{Name: step.Connection, Where: fmt.Sprintf("job %q %s", j.Name, name)})
				}
			}
			for i, p := range j.Plugins {
				if p.Connection != "" {
					out = append(out, ConnRef{Name: p.Connection, Type: PluginTypes[p.Type], Where: fmt.Sprintf("job %q plugin %d", j.Name, i+1)})
				}
			}
			for i, n := range j.Notify {
				out = append(out, ConnRef{Name: n.Connection, Where: fmt.Sprintf("job %q notify %d", j.Name, i+1)})
			}
		}
	}
	return out
}

// Plugin 是任务插件。
type Plugin struct {
	Name       string   `yaml:"name"`
	Type       string   `yaml:"type"`       // webhook / dingtalk / feishu / wecom / bark / telegram / email
	Connection string   `yaml:"connection"` // wecom / bark / telegram / email 使用的服务连接
	URL        string   `yaml:"url"`        // webhook / dingtalk / feishu 的地址，可含 ${SECRET}
	Secret     string   `yaml:"secret"`     // 钉钉 / 飞书加签密钥
	On         []string `yaml:"on"`         // success / failure / cancel / always，默认 [failure]
	To         []string `yaml:"to"`         // 邮件收件人
	Title      string   `yaml:"title"`
	Content    string   `yaml:"content"`
}

// ApprovalNotify 是审批消息的发送目标。
type ApprovalNotify struct {
	Connection string   `yaml:"connection"`
	To         []string `yaml:"to"`
}

// PluginTypes 是插件类型与其需要的服务连接类型（空为直接填地址）。
var PluginTypes = map[string]string{
	"webhook": "", "dingtalk": "", "feishu": "",
	"wecom": "wecom-bot", "bark": "bark", "telegram": "telegram", "email": "email",
}

var pluginEvents = []string{"success", "failure", "cancel", "always"}

func (j *Job) validatePlugins() error {
	if _, err := ParseCond(j.If); err != nil {
		return fmt.Errorf("if: %w", err)
	}
	for i := range j.Plugins {
		p := &j.Plugins[i]
		conn, ok := PluginTypes[p.Type]
		if !ok {
			return fmt.Errorf("plugins[%d]: unknown type %q", i, p.Type)
		}
		if conn != "" && p.Connection == "" {
			return fmt.Errorf("plugins[%d]: %s needs a service connection", i, p.Type)
		}
		if conn == "" && p.URL == "" {
			return fmt.Errorf("plugins[%d]: url is required", i)
		}
		if p.Type == "email" && len(p.To) == 0 {
			return fmt.Errorf("plugins[%d]: email needs recipients (to)", i)
		}
		if len(p.On) == 0 {
			p.On = []string{"failure"}
		}
		for _, on := range p.On {
			if !slices.Contains(pluginEvents, on) {
				return fmt.Errorf("plugins[%d]: on must be success, failure, cancel or always", i)
			}
		}
	}
	if len(j.Notify) > 0 && !j.IsApproval() {
		return errors.New("notify is only allowed on approval jobs")
	}
	for i, n := range j.Notify {
		if n.Connection == "" {
			return fmt.Errorf("notify[%d]: connection is required", i)
		}
	}
	return nil
}
