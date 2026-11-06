package trigger

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

var ErrSignature = errors.New("invalid webhook signature")

// Verify 校验请求来自持有 secret 的 Git 平台，按请求头识别平台：
//   - GitHub / Gitea / 通用：X-Hub-Signature-256 = "sha256=" + hex(HMAC-SHA256(secret, body))
//   - Gitea / Gogs：X-Gitea-Signature / X-Gogs-Signature = hex(HMAC-SHA256(secret, body))
//   - GitLab：X-Gitlab-Token = secret
//   - Gitee：X-Gitee-Token = secret（密码模式），或 base64(HMAC-SHA256(secret, timestamp+"\n"+secret))（签名模式）
func Verify(h http.Header, body []byte, secret string) error {
	if secret == "" {
		return ErrSignature
	}
	switch {
	case h.Get("X-Hub-Signature-256") != "":
		sig, ok := strings.CutPrefix(h.Get("X-Hub-Signature-256"), "sha256=")
		return checkHex(ok, sig, body, secret)
	case h.Get("X-Gitea-Signature") != "":
		return checkHex(true, h.Get("X-Gitea-Signature"), body, secret)
	case h.Get("X-Gogs-Signature") != "":
		return checkHex(true, h.Get("X-Gogs-Signature"), body, secret)
	case h.Get("X-Gitlab-Token") != "":
		return checkEqual(h.Get("X-Gitlab-Token"), secret)
	case h.Get("X-Gitee-Token") != "":
		token := h.Get("X-Gitee-Token")
		if checkEqual(token, secret) == nil {
			return nil
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(h.Get("X-Gitee-Timestamp") + "\n" + secret))
		return checkEqual(token, base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	}
	return fmt.Errorf("%w: no signature header", ErrSignature)
}

func checkHex(ok bool, sig string, body []byte, secret string) error {
	got, err := hex.DecodeString(sig)
	if !ok || err != nil {
		return ErrSignature
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return ErrSignature
	}
	return nil
}

func checkEqual(got, want string) error {
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return ErrSignature
	}
	return nil
}

// 归一化的事件类型。
const (
	KindPush        = "push"
	KindPullRequest = "pull_request"
	KindPing        = "ping"
)

// 归一化的 PR / MR 动作；其余动作（关闭、合并、编辑标题等）不触发运行。
const (
	PROpened      = "opened"
	PRReopened    = "reopened"
	PRSynchronize = "synchronize" // 源分支有新提交
)

// Event 是归一化后的 Webhook 事件。
type Event struct {
	Provider   string       // github / gitlab / gitee / gitea / gogs，按事件请求头判断，未识别时为空
	Name       string       // 平台原始事件名，未提供时为空
	Kind       string       // push / pull_request / ping / 其他（原样小写）
	DeliveryID string       // 平台的投递 ID，重试时不变，用于去重；平台未提供时为空
	Push       *Push        // 仅 Kind == push 时非空
	PR         *PullRequest // 仅 Kind == pull_request 时非空
}

// PullRequest 是从 GitHub / Gitea / Gogs / Gitee 的 Pull Request 与 GitLab 的 Merge Request 事件中提取的通用字段。
type PullRequest struct {
	Number       int
	Action       string // opened / reopened / synchronize，其他动作保留平台原值
	Title        string
	URL          string // PR 页面地址
	SourceBranch string
	TargetBranch string
	Commit       string // 源分支最新提交
	Ref          string // 目标仓库中指向 PR 最新提交的引用，如 refs/pull/5/head、refs/merge-requests/5/head
	Repo         string // 目标仓库 owner/name
	RepoURL      string // 目标仓库 HTTP 克隆地址
	SourceRepo   string
	Fork         bool // 来自 fork 仓库
	Author       string
}

// Triggers 判断该动作是否应触发运行（新建、重新打开、源分支有新提交）。
func (pr *PullRequest) Triggers() bool {
	switch pr.Action {
	case PROpened, PRReopened, PRSynchronize:
		return true
	}
	return false
}

// Push 是从各平台 push 事件中提取的通用字段。
type Push struct {
	Ref         string // refs/heads/main 或 refs/tags/v1.0
	Commit      string
	Repo        string // owner/name
	URL         string // HTTP 克隆地址
	Pusher      string
	Message     string // 最新提交说明的首行
	FullMessage string // 最新提交的完整说明（用于判断 [skip ci]）
	Deleted     bool   // 删除分支 / 标签
	// Files 是本次推送改动的文件（各提交的新增、修改、删除），FilesKnown 为 false 表示载荷中没有完整列表。
	Files      []string
	FilesKnown bool
	// 以下仅用于展示触发详情。
	Before       string
	CompareURL   string
	RepoWeb      string // 仓库网页地址
	Commits      []Commit
	TotalCommits int
}

// Commit 是推送载荷中的一个提交。
type Commit struct {
	ID      string
	Message string // 首行
	Author  string
	URL     string
}

func (p *Push) Branch() string {
	b, _ := strings.CutPrefix(p.Ref, "refs/heads/")
	if b == p.Ref {
		return ""
	}
	return b
}

func (p *Push) Tag() string {
	t, _ := strings.CutPrefix(p.Ref, "refs/tags/")
	if t == p.Ref {
		return ""
	}
	return t
}

var eventHeaders = []string{"X-GitHub-Event", "X-Gitlab-Event", "X-Gitee-Event", "X-Gitea-Event", "X-Gogs-Event"}

// deliveryHeaders 按优先级列出各平台的投递 ID 请求头（GitLab 的 Idempotency-Key 在重试间保持不变）。
// Gitee 不提供投递 ID。X-Flow-Delivery 供自定义脚本使用。
var deliveryHeaders = []string{"X-GitHub-Delivery", "Idempotency-Key", "X-Gitlab-Event-UUID", "X-Gitea-Delivery", "X-Gogs-Delivery", "X-Flow-Delivery"}

// EventKind 根据请求头识别事件：返回平台原始事件名与归一化类型，缺少事件头时按 push 处理（便于自定义脚本调用）。
func EventKind(h http.Header) (name, kind string) {
	for _, k := range eventHeaders {
		if v := h.Get(k); v != "" {
			name = v
			break
		}
	}
	switch strings.ToLower(name) {
	case "", "push", "push hook", "tag push hook":
		return name, KindPush
	case "pull_request", "merge request hook":
		return name, KindPullRequest
	}
	return name, strings.ToLower(name)
}

// ParseEvent 解析事件；push 与 PR / MR 事件同时解析载荷。
func ParseEvent(h http.Header, body []byte) (*Event, error) {
	ev := &Event{}
	ev.Name, ev.Kind = EventKind(h)
	for _, k := range eventHeaders {
		if h.Get(k) != "" {
			ev.Provider = strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(k, "X-"), "-Event"))
			break
		}
	}
	for _, k := range deliveryHeaders {
		if v := strings.TrimSpace(h.Get(k)); v != "" {
			ev.DeliveryID = v
			break
		}
	}
	switch ev.Kind {
	case KindPush:
	case KindPullRequest:
		pr, err := parsePullRequest(body)
		if err != nil {
			return nil, err
		}
		ev.PR = pr
		return ev, nil
	default:
		return ev, nil
	}
	push, err := parsePush(body)
	if err != nil {
		return nil, err
	}
	ev.Push = push
	return ev, nil
}

func parsePush(body []byte) (*Push, error) {
	var p struct {
		Ref         string `json:"ref"`
		Before      string `json:"before"`
		After       string `json:"after"`
		Compare     string `json:"compare"`      // GitHub
		CompareURL  string `json:"compare_url"`  // Gitea / Gogs
		CheckoutSHA string `json:"checkout_sha"` // GitLab 标签推送时 after 不是提交
		Deleted     bool   `json:"deleted"`
		Repository  struct {
			FullName   string `json:"full_name"`
			CloneURL   string `json:"clone_url"`
			GitHTTPURL string `json:"git_http_url"`
			HTMLURL    string `json:"html_url"`
			Homepage   string `json:"homepage"`
		} `json:"repository"`
		Project struct {
			PathWithNamespace string `json:"path_with_namespace"`
			GitHTTPURL        string `json:"git_http_url"`
			WebURL            string `json:"web_url"`
		} `json:"project"`
		Pusher struct {
			Name     string `json:"name"`
			Username string `json:"username"`
			Login    string `json:"login"`
		} `json:"pusher"`
		UserUsername string `json:"user_username"`
		HeadCommit   *struct {
			Message string `json:"message"`
		} `json:"head_commit"`
		Commits []struct {
			ID      string `json:"id"`
			Message string `json:"message"`
			URL     string `json:"url"`
			Author  struct {
				Name     string `json:"name"`
				Username string `json:"username"`
			} `json:"author"`
			Added    []string `json:"added"`
			Modified []string `json:"modified"`
			Removed  []string `json:"removed"`
		} `json:"commits"`
		TotalCommits int `json:"total_commits_count"` // GitLab
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("invalid push payload: %v", err)
	}
	if !strings.HasPrefix(p.Ref, "refs/") {
		return nil, fmt.Errorf("invalid push payload: unexpected ref %q", p.Ref)
	}
	push := &Push{
		Ref:    p.Ref,
		Commit: first(p.CheckoutSHA, p.After),
		Repo:   first(p.Repository.FullName, p.Project.PathWithNamespace),
		URL:    first(p.Repository.CloneURL, p.Project.GitHTTPURL, p.Repository.GitHTTPURL),
		Pusher: first(p.Pusher.Username, p.Pusher.Login, p.Pusher.Name, p.UserUsername),
	}
	push.Deleted = p.Deleted || strings.Trim(push.Commit, "0") == ""
	push.Before, push.CompareURL = p.Before, first(p.Compare, p.CompareURL)
	push.RepoWeb = first(p.Repository.HTMLURL, p.Project.WebURL, p.Repository.Homepage)
	push.TotalCommits = max(p.TotalCommits, len(p.Commits))
	for _, c := range p.Commits {
		push.Commits = append(push.Commits, Commit{ID: c.ID, Message: firstLine(c.Message), Author: first(c.Author.Username, c.Author.Name), URL: c.URL})
	}
	if p.HeadCommit != nil {
		push.Message = p.HeadCommit.Message
	} else if n := len(p.Commits); n > 0 {
		push.Message = p.Commits[n-1].Message
		for _, c := range p.Commits {
			if c.ID == push.Commit {
				push.Message = c.Message
			}
		}
	}
	push.FullMessage, push.Message = push.Message, firstLine(push.Message)
	// GitHub 载荷最多带 20 个提交、GitLab 带 total_commits_count：提交可能被截断时视为文件未知，不做路径过滤。
	n := len(p.Commits)
	push.FilesKnown = n > 0 && n < maxPayloadCommits && (p.TotalCommits == 0 || p.TotalCommits <= n)
	seen := map[string]bool{}
	for _, c := range p.Commits {
		for _, list := range [][]string{c.Added, c.Modified, c.Removed} {
			for _, f := range list {
				if !seen[f] {
					seen[f] = true
					push.Files = append(push.Files, f)
				}
			}
		}
	}
	return push, nil
}

const maxPayloadCommits = 20

// MatchPaths 按改动文件过滤推送：paths 非空时须有文件匹配其一；ignore 非空时若全部文件都被忽略则不触发。
// 改动文件未知（提交过多被截断、平台未提供）时不过滤。以 / 结尾的规则表示目录下的全部文件。
func MatchPaths(paths, ignore []string, p *Push) (bool, string) {
	if len(paths) == 0 && len(ignore) == 0 || !p.FilesKnown {
		return true, ""
	}
	relevant := p.Files[:0:0]
	for _, f := range p.Files {
		if !matchAny(dirPatterns(ignore), f) {
			relevant = append(relevant, f)
		}
	}
	if len(relevant) == 0 {
		return false, fmt.Sprintf("all %d changed file(s) match paths_ignore", len(p.Files))
	}
	if len(paths) == 0 {
		return true, ""
	}
	for _, f := range relevant {
		if matchAny(dirPatterns(paths), f) {
			return true, ""
		}
	}
	return false, fmt.Sprintf("none of the %d changed file(s) match paths", len(relevant))
}

func dirPatterns(patterns []string) []string {
	out := make([]string, len(patterns))
	for i, p := range patterns {
		if strings.HasSuffix(p, "/") {
			p += "**"
		}
		out[i] = p
	}
	return out
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

type prRepo struct {
	FullName   string `json:"full_name"`
	CloneURL   string `json:"clone_url"`
	GitHTTPURL string `json:"git_http_url"`
}

type prSide struct {
	Ref  string  `json:"ref"`
	SHA  string  `json:"sha"`
	Repo *prRepo `json:"repo"`
}

type prUser struct {
	Login    string `json:"login"`
	Username string `json:"username"`
	Name     string `json:"name"`
}

// parsePullRequest 解析 GitHub / Gitea / Gogs / Gitee（pull_request 对象）或 GitLab（object_attributes）的 PR / MR 事件。
func parsePullRequest(body []byte) (*PullRequest, error) {
	var p struct {
		Action      string `json:"action"`
		ActionDesc  string `json:"action_desc"` // Gitee：update 的具体原因
		Number      int    `json:"number"`
		PullRequest *struct {
			Number  int    `json:"number"`
			Title   string `json:"title"`
			HTMLURL string `json:"html_url"`
			Head    prSide `json:"head"`
			Base    prSide `json:"base"`
			User    prUser `json:"user"`
		} `json:"pull_request"`
		ObjectAttributes *struct {
			IID             int    `json:"iid"`
			Action          string `json:"action"`
			OldRev          string `json:"oldrev"`
			Title           string `json:"title"`
			URL             string `json:"url"`
			SourceBranch    string `json:"source_branch"`
			TargetBranch    string `json:"target_branch"`
			SourceProjectID int64  `json:"source_project_id"`
			TargetProjectID int64  `json:"target_project_id"`
			LastCommit      struct {
				ID string `json:"id"`
			} `json:"last_commit"`
		} `json:"object_attributes"`
		Project struct {
			PathWithNamespace string `json:"path_with_namespace"`
			GitHTTPURL        string `json:"git_http_url"`
		} `json:"project"`
		Source struct {
			PathWithNamespace string `json:"path_with_namespace"`
		} `json:"source"`
		Repository prRepo `json:"repository"`
		User       prUser `json:"user"`
		Sender     prUser `json:"sender"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("invalid pull request payload: %v", err)
	}
	var pr *PullRequest
	switch {
	case p.ObjectAttributes != nil: // GitLab
		a := p.ObjectAttributes
		pr = &PullRequest{
			Number: a.IID, Title: a.Title, URL: a.URL,
			SourceBranch: a.SourceBranch, TargetBranch: a.TargetBranch, Commit: a.LastCommit.ID,
			Ref:  fmt.Sprintf("refs/merge-requests/%d/head", a.IID),
			Repo: p.Project.PathWithNamespace, RepoURL: p.Project.GitHTTPURL,
			SourceRepo: first(p.Source.PathWithNamespace, p.Project.PathWithNamespace),
			Fork:       a.SourceProjectID != 0 && a.SourceProjectID != a.TargetProjectID,
			Author:     first(p.User.Username, p.User.Name),
		}
		switch a.Action {
		case "open":
			pr.Action = PROpened
		case "reopen":
			pr.Action = PRReopened
		case "update":
			pr.Action = "update" // 仅修改标题、描述等
			if a.OldRev != "" {
				pr.Action = PRSynchronize
			}
		default:
			pr.Action = a.Action
		}
	case p.PullRequest != nil:
		x := p.PullRequest
		pr = &PullRequest{
			Number: first(x.Number, p.Number), Title: x.Title, URL: x.HTMLURL,
			SourceBranch: x.Head.Ref, TargetBranch: x.Base.Ref, Commit: x.Head.SHA,
			Author: first(x.User.Login, x.User.Username, p.Sender.Login, p.Sender.Username),
		}
		pr.Ref = fmt.Sprintf("refs/pull/%d/head", pr.Number)
		base := p.Repository
		if x.Base.Repo != nil {
			base = *x.Base.Repo
		}
		pr.Repo, pr.RepoURL = first(base.FullName, p.Repository.FullName), first(base.CloneURL, base.GitHTTPURL)
		// 源仓库已删除时 head.repo 为空，按 fork 处理。
		if x.Head.Repo != nil {
			pr.SourceRepo = x.Head.Repo.FullName
		}
		pr.Fork = pr.SourceRepo == "" || !strings.EqualFold(pr.SourceRepo, pr.Repo)
		switch p.Action {
		case "opened", "open":
			pr.Action = PROpened
		case "reopened", "reopen":
			pr.Action = PRReopened
		case "synchronize", "synchronized":
			pr.Action = PRSynchronize
		case "update": // Gitee
			pr.Action = "update"
			if p.ActionDesc == "source_branch_changed" {
				pr.Action = PRSynchronize
			}
		default:
			pr.Action = p.Action
		}
	default:
		return nil, errors.New("invalid pull request payload: missing pull_request / object_attributes")
	}
	if pr.Number <= 0 || pr.TargetBranch == "" {
		return nil, errors.New("invalid pull request payload: missing number or target branch")
	}
	pr.Title = firstLine(pr.Title)
	return pr, nil
}

// Env 是注入到 PR 运行中的变量。FLOW_GIT_BRANCH 不设置，避免脚本把 PR 运行误当作分支推送（如据此部署）。
func (pr *PullRequest) Env() map[string]string {
	return map[string]string{
		"FLOW_GIT_EVENT":            KindPullRequest,
		"FLOW_GIT_REF":              pr.Ref,
		"FLOW_GIT_COMMIT":           pr.Commit,
		"FLOW_GIT_REPO":             pr.Repo,
		"FLOW_GIT_URL":              pr.RepoURL,
		"FLOW_GIT_PUSHER":           pr.Author,
		"FLOW_GIT_MESSAGE":          pr.Title,
		"FLOW_GIT_PR_NUMBER":        strconv.Itoa(pr.Number),
		"FLOW_GIT_PR_ACTION":        pr.Action,
		"FLOW_GIT_PR_URL":           pr.URL,
		"FLOW_GIT_PR_SOURCE_BRANCH": pr.SourceBranch,
		"FLOW_GIT_PR_TARGET_BRANCH": pr.TargetBranch,
	}
}

func first[T comparable](vals ...T) T {
	var zero T
	for _, v := range vals {
		if v != zero {
			return v
		}
	}
	return zero
}

// Env 是注入到运行中的 Git 相关变量。
func (p *Push) Env() map[string]string {
	env := map[string]string{
		"FLOW_GIT_EVENT":   KindPush,
		"FLOW_GIT_REF":     p.Ref,
		"FLOW_GIT_COMMIT":  p.Commit,
		"FLOW_GIT_REPO":    p.Repo,
		"FLOW_GIT_URL":     p.URL,
		"FLOW_GIT_PUSHER":  p.Pusher,
		"FLOW_GIT_MESSAGE": p.Message,
	}
	if b := p.Branch(); b != "" {
		env["FLOW_GIT_BRANCH"] = b
	}
	if t := p.Tag(); t != "" {
		env["FLOW_GIT_TAG"] = t
	}
	return env
}

// MatchRef 按分支 / 标签过滤规则判断是否触发，不触发时返回原因。
// 规则：branches 与 tags 都为空时匹配所有分支、不匹配标签；否则分支须匹配 branches、标签须匹配 tags。
// 通配符：* 匹配不含 / 的任意字符，** 匹配任意字符，? 匹配单个非 / 字符。
func MatchRef(branches, tags []string, p *Push) (bool, string) {
	switch {
	case p.Branch() != "":
		if len(branches) == 0 && len(tags) == 0 {
			return true, ""
		}
		if matchAny(branches, p.Branch()) {
			return true, ""
		}
		return false, fmt.Sprintf("branch %q does not match filter", p.Branch())
	case p.Tag() != "":
		if matchAny(tags, p.Tag()) {
			return true, ""
		}
		return false, fmt.Sprintf("tag %q does not match filter", p.Tag())
	}
	return false, fmt.Sprintf("ref %q is neither a branch nor a tag", p.Ref)
}

// MatchTarget 按分支过滤规则判断 PR 是否触发：branches 为空时匹配所有目标分支，否则目标分支须匹配其一。
func MatchTarget(branches []string, pr *PullRequest) (bool, string) {
	if len(branches) == 0 || matchAny(branches, pr.TargetBranch) {
		return true, ""
	}
	return false, fmt.Sprintf("target branch %q does not match filter", pr.TargetBranch)
}

func matchAny(patterns []string, name string) bool {
	for _, pat := range patterns {
		if re, err := globRegexp(pat); err == nil && re.MatchString(name) {
			return true
		}
	}
	return false
}

// ValidatePatterns 检查过滤规则是否合法。
func ValidatePatterns(patterns []string) error {
	for _, p := range patterns {
		if _, err := globRegexp(p); err != nil {
			return err
		}
	}
	return nil
}

func globRegexp(pattern string) (*regexp.Regexp, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, errors.New("empty ref pattern")
	}
	var b strings.Builder
	b.WriteString("^")
	rs := []rune(pattern)
	for i := 0; i < len(rs); i++ {
		switch c := rs[i]; {
		case c == '*' && i+2 < len(rs) && rs[i+1] == '*' && rs[i+2] == '/':
			b.WriteString("(?:.*/)?") // **/ 匹配零或多级目录，**/*.md 也匹配根目录下的文件
			i += 2
		case c == '*' && i+1 < len(rs) && rs[i+1] == '*':
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
