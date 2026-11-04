package trigger

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestParseCronErrors(t *testing.T) {
	for _, expr := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * * 13 *", "* * * * 8",
		"*/0 * * * *", "5-1 * * * *", "a * * * *", "* * * foo *"} {
		if _, err := ParseCron(expr, "UTC"); err == nil {
			t.Errorf("ParseCron(%q) succeeded", expr)
		}
	}
	if _, err := ParseCron("* * * * *", "Mars/Olympus"); err == nil {
		t.Error("invalid timezone accepted")
	}
}

func TestCronNext(t *testing.T) {
	sh, _ := time.LoadLocation("Asia/Shanghai")
	base := time.Date(2026, 1, 15, 10, 30, 20, 0, time.UTC) // 周四
	cases := []struct {
		expr, tz string
		after    time.Time
		want     time.Time
	}{
		{"* * * * *", "UTC", base, time.Date(2026, 1, 15, 10, 31, 0, 0, time.UTC)},
		{"*/15 * * * *", "UTC", base, time.Date(2026, 1, 15, 10, 45, 0, 0, time.UTC)},
		{"0 2 * * *", "UTC", base, time.Date(2026, 1, 16, 2, 0, 0, 0, time.UTC)},
		{"@hourly", "UTC", base, time.Date(2026, 1, 15, 11, 0, 0, 0, time.UTC)},
		{"0 9 * * mon-fri", "UTC", time.Date(2026, 1, 16, 10, 0, 0, 0, time.UTC), time.Date(2026, 1, 19, 9, 0, 0, 0, time.UTC)},
		{"0 0 * * 7", "UTC", base, time.Date(2026, 1, 18, 0, 0, 0, 0, time.UTC)},
		{"0 0 29 2 *", "UTC", base, time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC)},
		{"0 0 1,15 jan,jul *", "UTC", base, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
		{"30 8 5/10 * *", "UTC", base, time.Date(2026, 1, 25, 8, 30, 0, 0, time.UTC)},
		// 日与周同时限定时取并集：1 号或周一。
		{"0 0 1 * 1", "UTC", base, time.Date(2026, 1, 19, 0, 0, 0, 0, time.UTC)},
		// 时区：上海 02:00 = UTC 18:00。
		{"0 2 * * *", "Asia/Shanghai", base, time.Date(2026, 1, 16, 2, 0, 0, 0, sh)},
		{"0 0 30 2 *", "UTC", base, time.Time{}},
	}
	for _, c := range cases {
		s, err := ParseCron(c.expr, c.tz)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if got := s.Next(c.after); !got.Equal(c.want) {
			t.Errorf("Next(%q, %s) = %v, want %v", c.expr, c.after, got, c.want)
		}
	}
}

func TestCronNextDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip(err)
	}
	// 2026-03-08 02:00 拨快到 03:00：02:30 不存在，顺延。
	s, _ := ParseCron("30 2 * * *", "America/New_York")
	got := s.Next(time.Date(2026, 3, 8, 0, 0, 0, 0, ny))
	if got.Before(time.Date(2026, 3, 8, 3, 0, 0, 0, ny)) || got.After(time.Date(2026, 3, 9, 2, 30, 0, 0, ny)) {
		t.Errorf("spring forward: %v", got)
	}
	// 2026-11-01 02:00 回拨到 01:00：01:30 只触发一次。
	s, _ = ParseCron("30 1 * * *", "America/New_York")
	firstFire := s.Next(time.Date(2026, 11, 1, 0, 0, 0, 0, ny))
	second := s.Next(firstFire)
	if second.Day() != 2 {
		t.Errorf("fall back fired twice: %v then %v", firstFire, second)
	}
}

func hmacHex(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

func TestVerify(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main"}`)
	gitee := hmac.New(sha256.New, []byte("s3cret"))
	gitee.Write([]byte("1700000000000\ns3cret"))
	ok := []http.Header{
		{"X-Hub-Signature-256": {"sha256=" + hmacHex("s3cret", string(body))}},
		{"X-Gitea-Signature": {hmacHex("s3cret", string(body))}},
		{"X-Gogs-Signature": {hmacHex("s3cret", string(body))}},
		{"X-Gitlab-Token": {"s3cret"}},
		{"X-Gitee-Token": {"s3cret"}},
		{"X-Gitee-Token": {base64.StdEncoding.EncodeToString(gitee.Sum(nil))}, "X-Gitee-Timestamp": {"1700000000000"}},
	}
	for _, h := range ok {
		if err := Verify(h, body, "s3cret"); err != nil {
			t.Errorf("Verify(%v) = %v", h, err)
		}
	}
	bad := []http.Header{
		{},
		{"X-Hub-Signature-256": {hmacHex("s3cret", string(body))}}, // 缺少 sha256= 前缀
		{"X-Hub-Signature-256": {"sha256=" + hmacHex("other", string(body))}},
		{"X-Hub-Signature-256": {"sha256=zz"}},
		{"X-Gitlab-Token": {"nope"}},
		{"X-Gitee-Token": {"nope"}, "X-Gitee-Timestamp": {"1"}},
	}
	for _, h := range bad {
		if err := Verify(h, body, "s3cret"); !errors.Is(err, ErrSignature) {
			t.Errorf("Verify(%v) = %v, want ErrSignature", h, err)
		}
	}
	if err := Verify(http.Header{"X-Gitlab-Token": {""}}, body, ""); err == nil {
		t.Error("empty secret accepted")
	}
}

func TestParseEvent(t *testing.T) {
	github := `{"ref":"refs/heads/main","after":"abc123","repository":{"full_name":"acme/app","clone_url":"https://github.com/acme/app.git"},
		"pusher":{"name":"alice"},"head_commit":{"message":"fix: bug\n\nbody"}}`
	ev, err := ParseEvent(http.Header{"X-Github-Event": {"push"}}, []byte(github))
	if err != nil || ev.Kind != "push" {
		t.Fatalf("github: %+v, %v", ev, err)
	}
	p := ev.Push
	if p.Branch() != "main" || p.Commit != "abc123" || p.Repo != "acme/app" || p.URL != "https://github.com/acme/app.git" ||
		p.Pusher != "alice" || p.Message != "fix: bug" || p.Deleted {
		t.Errorf("github push = %+v", p)
	}
	env := p.Env()
	if env["FLOW_GIT_BRANCH"] != "main" || env["FLOW_GIT_COMMIT"] != "abc123" || env["FLOW_GIT_TAG"] != "" {
		t.Errorf("env = %v", env)
	}

	gitlab := `{"ref":"refs/tags/v1.0","after":"tagobj","checkout_sha":"c0ffee","user_username":"bob",
		"project":{"path_with_namespace":"grp/app","git_http_url":"https://gitlab.example.com/grp/app.git"},
		"commits":[{"id":"x","message":"old"},{"id":"c0ffee","message":"release"}]}`
	ev, err = ParseEvent(http.Header{"X-Gitlab-Event": {"Tag Push Hook"}}, []byte(gitlab))
	if err != nil {
		t.Fatal(err)
	}
	p = ev.Push
	if p.Tag() != "v1.0" || p.Commit != "c0ffee" || p.Repo != "grp/app" || p.Pusher != "bob" || p.Message != "release" {
		t.Errorf("gitlab push = %+v", p)
	}

	del := `{"ref":"refs/heads/old","after":"0000000000000000000000000000000000000000"}`
	if ev, _ := ParseEvent(http.Header{"X-Gitee-Event": {"Push Hook"}}, []byte(del)); !ev.Push.Deleted {
		t.Error("branch deletion not detected")
	}
	if ev, _ := ParseEvent(http.Header{"X-Github-Event": {"ping"}}, []byte(`{}`)); ev.Kind != "ping" || ev.Push != nil {
		t.Errorf("ping = %+v", ev)
	}
	if _, err := ParseEvent(http.Header{"X-Gitlab-Event": {"Merge Request Hook"}}, []byte(`{}`)); err == nil {
		t.Error("empty merge request payload accepted")
	}
	if ev, _ := ParseEvent(http.Header{"X-Github-Event": {"issues"}}, []byte(`{}`)); ev.Kind != "issues" || ev.Push != nil || ev.PR != nil {
		t.Errorf("issues = %+v", ev)
	}
	for _, body := range []string{`not json`, `{"ref":"main"}`} {
		if _, err := ParseEvent(http.Header{}, []byte(body)); err == nil {
			t.Errorf("ParseEvent(%s) succeeded", body)
		}
	}
}

func TestParsePullRequest(t *testing.T) {
	cases := []struct {
		name   string
		header http.Header
		body   string
		want   PullRequest
	}{
		{
			name:   "github same repo",
			header: http.Header{"X-Github-Event": {"pull_request"}, "X-Github-Delivery": {"d-1"}},
			body: `{"action":"opened","number":7,"pull_request":{"number":7,"title":"Add feature\nmore","html_url":"https://github.com/acme/app/pull/7",
				"user":{"login":"alice"},"head":{"ref":"feat","sha":"h1","repo":{"full_name":"acme/app"}},
				"base":{"ref":"main","repo":{"full_name":"acme/app","clone_url":"https://github.com/acme/app.git"}}},
				"repository":{"full_name":"acme/app","clone_url":"https://github.com/acme/app.git"}}`,
			want: PullRequest{Number: 7, Action: PROpened, Title: "Add feature", URL: "https://github.com/acme/app/pull/7",
				SourceBranch: "feat", TargetBranch: "main", Commit: "h1", Ref: "refs/pull/7/head", Repo: "acme/app",
				RepoURL: "https://github.com/acme/app.git", SourceRepo: "acme/app", Author: "alice"},
		},
		{
			name:   "github fork synchronize",
			header: http.Header{"X-Github-Event": {"pull_request"}},
			body: `{"action":"synchronize","number":8,"pull_request":{"number":8,"head":{"ref":"main","sha":"h2","repo":{"full_name":"mallory/app"}},
				"base":{"ref":"main","repo":{"full_name":"acme/app"}},"user":{"login":"mallory"}},"repository":{"full_name":"acme/app"}}`,
			want: PullRequest{Number: 8, Action: PRSynchronize, SourceBranch: "main", TargetBranch: "main", Commit: "h2",
				Ref: "refs/pull/8/head", Repo: "acme/app", SourceRepo: "mallory/app", Fork: true, Author: "mallory"},
		},
		{
			name:   "github closed",
			header: http.Header{"X-Github-Event": {"pull_request"}},
			body:   `{"action":"closed","number":9,"pull_request":{"number":9,"head":{"ref":"x","repo":{"full_name":"a/b"}},"base":{"ref":"main","repo":{"full_name":"a/b"}}}}`,
			want:   PullRequest{Number: 9, Action: "closed", SourceBranch: "x", TargetBranch: "main", Ref: "refs/pull/9/head", Repo: "a/b", SourceRepo: "a/b"},
		},
		{
			name:   "gitea synchronized",
			header: http.Header{"X-Gitea-Event": {"pull_request"}, "X-Gitea-Delivery": {"d-2"}},
			body: `{"action":"synchronized","number":3,"pull_request":{"number":3,"title":"t","head":{"ref":"f","sha":"g1","repo":{"full_name":"o/r"}},
				"base":{"ref":"dev","repo":{"full_name":"o/r","clone_url":"https://gitea.example.com/o/r.git"}},"user":{"username":"carol"}}}`,
			want: PullRequest{Number: 3, Action: PRSynchronize, Title: "t", SourceBranch: "f", TargetBranch: "dev", Commit: "g1",
				Ref: "refs/pull/3/head", Repo: "o/r", RepoURL: "https://gitea.example.com/o/r.git", SourceRepo: "o/r", Author: "carol"},
		},
		{
			name:   "gitee source branch changed",
			header: http.Header{"X-Gitee-Event": {"Merge Request Hook"}},
			body: `{"action":"update","action_desc":"source_branch_changed","pull_request":{"number":4,"title":"t","head":{"ref":"f","sha":"e1","repo":{"full_name":"o/r"}},
				"base":{"ref":"master","repo":{"full_name":"o/r","git_http_url":"https://gitee.com/o/r.git"}},"user":{"login":"dave"}}}`,
			want: PullRequest{Number: 4, Action: PRSynchronize, Title: "t", SourceBranch: "f", TargetBranch: "master", Commit: "e1",
				Ref: "refs/pull/4/head", Repo: "o/r", RepoURL: "https://gitee.com/o/r.git", SourceRepo: "o/r", Author: "dave"},
		},
		{
			name:   "gitlab update with new commits",
			header: http.Header{"X-Gitlab-Event": {"Merge Request Hook"}, "Idempotency-Key": {"d-3"}, "X-Gitlab-Event-Uuid": {"ignored"}},
			body: `{"object_kind":"merge_request","user":{"username":"erin"},"project":{"path_with_namespace":"grp/app","git_http_url":"https://gitlab.example.com/grp/app.git"},
				"source":{"path_with_namespace":"grp/app"},"object_attributes":{"iid":12,"action":"update","oldrev":"o1","title":"MR","url":"https://gitlab.example.com/grp/app/-/merge_requests/12",
				"source_branch":"feat","target_branch":"main","source_project_id":5,"target_project_id":5,"last_commit":{"id":"l1"}}}`,
			want: PullRequest{Number: 12, Action: PRSynchronize, Title: "MR", URL: "https://gitlab.example.com/grp/app/-/merge_requests/12",
				SourceBranch: "feat", TargetBranch: "main", Commit: "l1", Ref: "refs/merge-requests/12/head", Repo: "grp/app",
				RepoURL: "https://gitlab.example.com/grp/app.git", SourceRepo: "grp/app", Author: "erin"},
		},
		{
			name:   "gitlab fork title edit",
			header: http.Header{"X-Gitlab-Event": {"Merge Request Hook"}},
			body: `{"object_attributes":{"iid":13,"action":"update","source_branch":"x","target_branch":"main","source_project_id":9,"target_project_id":5},
				"project":{"path_with_namespace":"grp/app"},"source":{"path_with_namespace":"zed/app"}}`,
			want: PullRequest{Number: 13, Action: "update", SourceBranch: "x", TargetBranch: "main", Ref: "refs/merge-requests/13/head",
				Repo: "grp/app", SourceRepo: "zed/app", Fork: true},
		},
	}
	for _, c := range cases {
		ev, err := ParseEvent(c.header, []byte(c.body))
		if err != nil || ev.Kind != KindPullRequest || ev.PR == nil {
			t.Errorf("%s: %+v, %v", c.name, ev, err)
			continue
		}
		if *ev.PR != c.want {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, *ev.PR, c.want)
		}
	}

	ids := map[string]http.Header{
		"d-1": {"X-Github-Event": {"push"}, "X-Github-Delivery": {"d-1"}},
		"d-3": {"X-Gitlab-Event": {"Push Hook"}, "Idempotency-Key": {"d-3"}, "X-Gitlab-Event-Uuid": {"u"}},
		"u":   {"X-Gitlab-Event": {"Push Hook"}, "X-Gitlab-Event-Uuid": {"u"}},
		"":    {"X-Gitee-Event": {"Push Hook"}},
	}
	for want, h := range ids {
		ev, err := ParseEvent(h, []byte(`{"ref":"refs/heads/main","after":"a"}`))
		if err != nil || ev.DeliveryID != want {
			t.Errorf("delivery id for %v = %q, %v; want %q", h, ev.DeliveryID, err, want)
		}
	}

	pr := &PullRequest{Number: 7, Action: PROpened, Ref: "refs/pull/7/head", Commit: "h1", SourceBranch: "feat", TargetBranch: "main"}
	env := pr.Env()
	if env["FLOW_GIT_EVENT"] != "pull_request" || env["FLOW_GIT_PR_NUMBER"] != "7" || env["FLOW_GIT_REF"] != "refs/pull/7/head" ||
		env["FLOW_GIT_PR_TARGET_BRANCH"] != "main" || env["FLOW_GIT_BRANCH"] != "" {
		t.Errorf("pr env = %v", env)
	}
	if ok, _ := MatchTarget([]string{"release/*"}, pr); ok {
		t.Error("target main matched release/*")
	}
	if ok, _ := MatchTarget(nil, pr); !ok {
		t.Error("empty filter should match all targets")
	}
}

func TestMatchRef(t *testing.T) {
	br := func(name string) *Push { return &Push{Ref: "refs/heads/" + name} }
	tag := func(name string) *Push { return &Push{Ref: "refs/tags/" + name} }
	cases := []struct {
		branches, tags []string
		push           *Push
		want           bool
	}{
		{nil, nil, br("anything"), true},
		{nil, nil, tag("v1"), false},
		{[]string{"main"}, nil, br("main"), true},
		{[]string{"main"}, nil, br("dev"), false},
		{[]string{"release/*"}, nil, br("release/1.0"), true},
		{[]string{"release/*"}, nil, br("release/1.0/hotfix"), false},
		{[]string{"feature/**"}, nil, br("feature/a/b"), true},
		{[]string{"发布-*"}, nil, br("发布-一"), true},
		{nil, []string{"v*"}, br("main"), false},
		{nil, []string{"v*"}, tag("v1.2.0"), true},
		{[]string{"main"}, []string{"v?.*"}, tag("v1.2"), true},
		{[]string{"a.b"}, nil, br("axb"), false}, // . 按字面匹配
	}
	for _, c := range cases {
		if got, reason := MatchRef(c.branches, c.tags, c.push); got != c.want {
			t.Errorf("MatchRef(%v, %v, %s) = %v (%s)", c.branches, c.tags, c.push.Ref, got, reason)
		}
	}
	if ValidatePatterns([]string{"main", " "}) == nil {
		t.Error("blank pattern accepted")
	}
}

func TestMatchPaths(t *testing.T) {
	body := `{"ref":"refs/heads/main","after":"abc","commits":[
		{"id":"1","added":["services/api/main.go"],"modified":["README.md"]},
		{"id":"abc","modified":["docs/guide.md"],"removed":["services/api/old.go"]}]}`
	ev, err := ParseEvent(http.Header{"X-Github-Event": {"push"}}, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	p := ev.Push
	if !p.FilesKnown || len(p.Files) != 4 {
		t.Fatalf("files = %v known %v", p.Files, p.FilesKnown)
	}
	for _, c := range []struct {
		paths, ignore []string
		want          bool
	}{
		{nil, nil, true},
		{[]string{"services/api/"}, nil, true},
		{[]string{"services/web/**"}, nil, false},
		{nil, []string{"*.md", "docs/"}, true},         // 还有 services 下的改动
		{nil, []string{"**"}, false},                   // 全部忽略
		{[]string{"**/*.md"}, []string{"docs/"}, true}, // **/ 也匹配根目录，README.md 命中
		{[]string{"*.md"}, []string{"docs/"}, true},    // README.md 在根目录
		{[]string{"services/**"}, []string{"services/api/**"}, false},
	} {
		if ok, reason := MatchPaths(c.paths, c.ignore, p); ok != c.want {
			t.Errorf("paths %v ignore %v = %v (%s), want %v", c.paths, c.ignore, ok, reason, c.want)
		}
	}
	// 提交被截断或未提供文件列表时不过滤。
	p.FilesKnown = false
	if ok, _ := MatchPaths([]string{"nothing/"}, nil, p); !ok {
		t.Error("unknown files should not be filtered")
	}
}
