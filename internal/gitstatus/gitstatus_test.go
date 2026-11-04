package gitstatus

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	for in, want := range map[[2]string]Repo{
		{"https://github.com/acme/app.git", ""}:             {GitHub, "https://github.com", "acme/app"},
		{"git@github.com:acme/app.git", ""}:                 {GitHub, "https://github.com", "acme/app"},
		{"https://gitlab.example.com/g/sub/proj", ""}:       {GitLab, "https://gitlab.example.com", "g/sub/proj"},
		{"ssh://git@gitea.local:2222/o/r.git", ""}:          {Gitea, "https://gitea.local", "o/r"},
		{"http://10.0.0.5:3000/o/r.git", "gitea"}:           {Gitea, "http://10.0.0.5:3000", "o/r"},
		{"https://code.example.com/team/svc.git", "github"}: {GitHub, "https://code.example.com", "team/svc"},
	} {
		got, err := Parse(in[0], in[1])
		if err != nil || *got != want {
			t.Errorf("Parse(%q, %q) = %+v, %v; want %+v", in[0], in[1], got, err, want)
		}
	}
	for _, in := range [][2]string{{"https://code.example.com/o/r", ""}, {"https://github.com/o/r", "bitbucket"}, {"https://github.com/onlyowner", ""}} {
		if _, err := Parse(in[0], in[1]); err == nil {
			t.Errorf("Parse(%q, %q) should fail", in[0], in[1])
		}
	}
}

func TestPost(t *testing.T) {
	type call struct {
		Path, Auth, Token string
		Body              map[string]string
	}
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		calls = append(calls, call{r.URL.EscapedPath(), r.Header.Get("Authorization"), r.Header.Get("PRIVATE-TOKEN"), body})
		if strings.Contains(r.URL.Path, "denied") {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	c := &Client{}
	ctx := context.Background()
	st := Status{State: Failed, Context: "flow/app", Description: "构建失败", TargetURL: "http://flow/#/runs/1"}
	for _, p := range Providers {
		r, _ := Parse(srv.URL+"/g/sub/app.git", p)
		if err := c.Post(ctx, r, "tok", "abc123", st); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
	want := []call{
		{"/api/v3/repos/g/sub/app/statuses/abc123", "Bearer tok", "", map[string]string{"state": "failure", "context": "flow/app", "description": "构建失败", "target_url": "http://flow/#/runs/1"}},
		{"/api/v4/projects/g%2Fsub%2Fapp/statuses/abc123", "", "tok", map[string]string{"state": "failed", "name": "flow/app", "description": "构建失败", "target_url": "http://flow/#/runs/1"}},
		{"/api/v1/repos/g/sub/app/statuses/abc123", "token tok", "", map[string]string{"state": "failure", "context": "flow/app", "description": "构建失败", "target_url": "http://flow/#/runs/1"}},
	}
	for i := range want {
		got, _ := json.Marshal(calls[i])
		exp, _ := json.Marshal(want[i])
		if string(got) != string(exp) {
			t.Errorf("call %d = %s\nwant %s", i, got, exp)
		}
	}
	r, _ := Parse(srv.URL+"/o/denied", Gitea)
	if err := c.Post(ctx, r, "bad", "abc", st); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("denied err = %v", err)
	}
}
