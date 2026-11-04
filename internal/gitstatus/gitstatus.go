// Package gitstatus 把运行结果回写为 Git 平台上的提交状态（GitHub / GitLab / Gitea），
// 在提交与合并请求页面显示检查结果。
package gitstatus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// 支持的平台。
const (
	GitHub = "github"
	GitLab = "gitlab"
	Gitea  = "gitea"
)

var Providers = []string{GitHub, GitLab, Gitea}

// State 是与平台无关的状态。
type State string

const (
	Pending  State = "pending"
	Running  State = "running"
	Success  State = "success"
	Failed   State = "failed"
	Canceled State = "canceled"
)

// Status 是一次状态回写。
type Status struct {
	State       State
	Context     string // 检查名称，如 flow/build
	Description string
	TargetURL   string // 运行详情页，可为空
}

// Repo 是从仓库地址解析出的平台信息。
type Repo struct {
	Provider string
	Base     string // 平台根地址，如 https://gitlab.example.com
	Path     string // owner/repo 或 group/sub/project
}

// Parse 解析仓库地址（https://host/owner/repo(.git)、git@host:owner/repo.git、ssh://git@host/owner/repo）。
// provider 为空时按主机名推断：github.com、包含 gitlab / gitea 的主机名；无法推断时返回错误。
func Parse(repoURL, provider string) (*Repo, error) {
	raw := strings.TrimSpace(repoURL)
	scheme := "https"
	var host, path string
	switch {
	case strings.Contains(raw, "://"):
		u, err := url.Parse(raw)
		if err != nil {
			return nil, err
		}
		if u.Scheme == "http" {
			scheme = "http"
		}
		host, path = u.Host, u.Path
		if u.Scheme == "ssh" || u.Scheme == "git" {
			host = u.Hostname() // SSH 端口与 HTTP 端口无关
		}
	case strings.Contains(raw, "@") && strings.Contains(raw, ":"):
		at := raw[strings.Index(raw, "@")+1:]
		host, path, _ = strings.Cut(at, ":")
	default:
		return nil, fmt.Errorf("unsupported repository url %q", repoURL)
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if host == "" || !strings.Contains(path, "/") {
		return nil, fmt.Errorf("repository url %q must look like https://host/owner/repo", repoURL)
	}
	if provider == "" {
		h := strings.ToLower(host)
		switch {
		case h == "github.com" || h == "www.github.com":
			provider = GitHub
		case strings.Contains(h, "gitlab"):
			provider = GitLab
		case strings.Contains(h, "gitea"):
			provider = Gitea
		default:
			return nil, fmt.Errorf("cannot tell the git platform of %s; set source.provider to one of %s", host, strings.Join(Providers, ", "))
		}
	}
	switch provider {
	case GitHub, GitLab, Gitea:
	default:
		return nil, fmt.Errorf("unknown provider %q (want %s)", provider, strings.Join(Providers, ", "))
	}
	return &Repo{Provider: provider, Base: scheme + "://" + host, Path: path}, nil
}

// Client 发送状态请求。
type Client struct {
	HTTP *http.Client
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// Post 为提交 sha 写入状态。
func (c *Client) Post(ctx context.Context, r *Repo, token, sha string, st Status) error {
	var (
		endpoint string
		body     any
		header   = http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json"}}
	)
	switch r.Provider {
	case GitHub:
		api := "https://api.github.com"
		if !strings.EqualFold(strings.TrimPrefix(r.Base, "https://"), "github.com") {
			api = r.Base + "/api/v3" // GitHub Enterprise Server
		}
		endpoint = fmt.Sprintf("%s/repos/%s/statuses/%s", api, r.Path, sha)
		header.Set("Authorization", "Bearer "+token)
		body = map[string]string{"state": githubState(st.State), "context": st.Context, "description": trim(st.Description), "target_url": st.TargetURL}
	case GitLab:
		endpoint = fmt.Sprintf("%s/api/v4/projects/%s/statuses/%s", r.Base, url.PathEscape(r.Path), sha)
		header.Set("PRIVATE-TOKEN", token)
		body = map[string]string{"state": gitlabState(st.State), "name": st.Context, "description": trim(st.Description), "target_url": st.TargetURL}
	case Gitea:
		endpoint = fmt.Sprintf("%s/api/v1/repos/%s/statuses/%s", r.Base, r.Path, sha)
		header.Set("Authorization", "token "+token)
		body = map[string]string{"state": githubState(st.State), "context": st.Context, "description": trim(st.Description), "target_url": st.TargetURL}
	default:
		return fmt.Errorf("unknown provider %q", r.Provider)
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header = header
	res, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("%s %s: %s %s", r.Provider, endpoint, res.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// GitHub 与 Gitea 没有 running / canceled。
func githubState(s State) string {
	switch s {
	case Success:
		return "success"
	case Failed:
		return "failure"
	case Canceled:
		return "error"
	}
	return "pending"
}

func gitlabState(s State) string {
	switch s {
	case Success:
		return "success"
	case Failed:
		return "failed"
	case Canceled:
		return "canceled"
	case Running:
		return "running"
	}
	return "pending"
}

// GitHub 限制描述 140 字符。
func trim(s string) string {
	if r := []rune(s); len(r) > 140 {
		return string(r[:139]) + "…"
	}
	return s
}
