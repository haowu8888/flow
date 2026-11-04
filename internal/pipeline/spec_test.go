package pipeline

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseDefaultsAndDependencies(t *testing.T) {
	spec, err := Parse([]byte(`
name: demo
stages:
  - name: build
    jobs:
      - name: a
        steps: [{run: echo a}]
      - name: b
        steps: [{run: echo b}]
  - name: test
    jobs:
      - name: c
        steps: [{run: echo c}]
      - name: d
        needs: [a]
        steps: [{run: echo d}]
      - name: e
        needs: []
        steps: [{run: echo e}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := spec.Stages[0].Jobs[0].Steps[0].Name; got != "step-1" {
		t.Errorf("default step name = %q", got)
	}
	want := map[string][]string{
		"a": nil, "b": nil,
		"c": {"a", "b"},
		"d": {"a"},
		"e": {},
	}
	if got := spec.Dependencies(); !reflect.DeepEqual(got, want) {
		t.Errorf("deps = %v, want %v", got, want)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]struct{ yaml, want string }{
		"unknown field": {`
name: x
stages: [{name: s, jobs: [{name: a, scripts: [], steps: [{run: x}]}]}]`, "field scripts not found"},
		"duplicate job": {`
name: x
stages:
  - {name: s1, jobs: [{name: a, steps: [{run: x}]}]}
  - {name: s2, jobs: [{name: a, steps: [{run: x}]}]}`, `duplicate job "a"`},
		"unknown need": {`
name: x
stages: [{name: s, jobs: [{name: a, needs: [zzz], steps: [{run: x}]}]}]`, `unknown job "zzz"`},
		"later stage": {`
name: x
stages:
  - {name: s1, jobs: [{name: a, needs: [b], steps: [{run: x}]}]}
  - {name: s2, jobs: [{name: b, steps: [{run: x}]}]}`, "later stage"},
		"cycle": {`
name: x
stages:
  - name: s
    jobs:
      - {name: a, needs: [b], steps: [{run: x}]}
      - {name: b, needs: [a], steps: [{run: x}]}`, "dependency cycle"},
		"empty step": {`
name: x
stages: [{name: s, jobs: [{name: a, steps: [{run: "  "}]}]}]`, "run or uses is required"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestParseAdvancedJobOptions(t *testing.T) {
	spec, err := Parse([]byte(`
name: adv
source: {url: https://git.example.com/acme/app.git, branch: main, token_secret: GIT_TOKEN}
notify:
  - {type: dingtalk, url: "https://oapi.dingtalk.com/robot/send?access_token=${DING}"}
stages:
  - name: test
    jobs:
      - name: unit
        runs-on: linux
        matrix: {GO: [1.22, "1.23"], OS: [alpine]}
        timeout: 10m
        retry: 2
        artifacts: [dist/, "reports/*.xml"]
        cache: {key: "go-$GO", paths: [.cache]}
        steps: [{run: go test}]
  - name: gate
    jobs:
      - {name: approve, type: approval, approvers: [alice]}
  - name: deploy
    jobs:
      - {name: prod, needs: [unit, approve], runs-on: [linux, prod], steps: [{run: deploy}]}
      - {name: cleanup, when: always, checkout: false, steps: [{run: cleanup}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	unit := spec.Stages[0].Jobs
	if len(unit) != 2 || unit[0].Name != "unit (1.22, alpine)" || unit[1].Env["GO"] != "1.23" || unit[0].Env["OS"] != "alpine" {
		t.Fatalf("matrix = %+v", unit)
	}
	if unit[0].TimeoutDuration.Minutes() != 10 || unit[0].RunsOn[0] != "linux" || unit[0].When != WhenOnSuccess || unit[0].MatrixValues[0] != "GO=1.22" {
		t.Errorf("unit options = %+v", unit[0])
	}
	deps := spec.Dependencies()
	if want := []string{"unit (1.22, alpine)", "unit (1.23, alpine)", "approve"}; !reflect.DeepEqual(deps["prod"], want) {
		t.Errorf("prod deps = %v", deps["prod"])
	}
	if want := []string{"unit (1.22, alpine)", "unit (1.23, alpine)"}; !reflect.DeepEqual(deps["approve"], want) {
		t.Errorf("approve deps = %v", deps["approve"])
	}
	if !spec.Stages[1].Jobs[0].IsApproval() || spec.Notify[0].On[0] != "failed" || spec.Source.TokenSecret != "GIT_TOKEN" {
		t.Errorf("spec = %+v", spec)
	}
}

func TestParseAdvancedErrors(t *testing.T) {
	base := "name: x\nstages:\n  - name: s\n    jobs:\n      - "
	cases := map[string]string{
		"approval with steps": `{name: a, type: approval, steps: [{run: x}]}`,
		"unknown type":        `{name: a, type: deploy, steps: [{run: x}]}`,
		"bad when":            `{name: a, when: sometimes, steps: [{run: x}]}`,
		"bad timeout":         `{name: a, timeout: soon, steps: [{run: x}]}`,
		"retry too big":       `{name: a, retry: 9, steps: [{run: x}]}`,
		"artifact escape":     `{name: a, artifacts: [../etc], steps: [{run: x}]}`,
		"absolute cache":      `{name: a, cache: {key: k, paths: [/root]}, steps: [{run: x}]}`,
		"cache without key":   `{name: a, cache: {paths: [x]}, steps: [{run: x}]}`,
		"empty matrix":        `{name: a, matrix: {GO: []}, steps: [{run: x}]}`,
		"bad matrix key":      `{name: a, matrix: {"go-v": [1]}, steps: [{run: x}]}`,
		"huge matrix":         `{name: a, matrix: {A: [1,2,3,4,5,6,7,8], B: [1,2,3,4,5,6,7,8,9]}, steps: [{run: x}]}`,
	}
	for name, job := range cases {
		if _, err := Parse([]byte(base + job)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	for name, doc := range map[string]string{
		"notify type":   "name: x\nnotify: [{type: email, url: x}]\nstages: [{name: s, jobs: [{name: a, steps: [{run: x}]}]}]",
		"notify on":     "name: x\nnotify: [{type: webhook, url: x, on: [started]}]\nstages: [{name: s, jobs: [{name: a, steps: [{run: x}]}]}]",
		"source no url": "name: x\nsource: {branch: main}\nstages: [{name: s, jobs: [{name: a, steps: [{run: x}]}]}]",
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestRetention(t *testing.T) {
	cases := map[string]time.Duration{"7d": 7 * 24 * time.Hour, "36h": 36 * time.Hour, "0": ForeverTTL, " 1d ": 24 * time.Hour}
	for in, want := range cases {
		if got, err := ParseRetention(in); err != nil || got != want {
			t.Errorf("ParseRetention(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "-1h", "1.5d", "0d", "d", "forever", "01d"} {
		if _, err := ParseRetention(bad); err == nil {
			t.Errorf("ParseRetention(%q) accepted", bad)
		}
	}
	spec, err := Parse([]byte("name: p\nretention: {artifacts: 3d, runs: 50}\nstages: [{name: s, jobs: [{name: j, steps: [{run: x}]}]}]\n"))
	if err != nil || spec.Retention.ArtifactTTL != 72*time.Hour || spec.Retention.Runs != 50 {
		t.Fatalf("spec = %+v, %v", spec.Retention, err)
	}
	for _, bad := range []string{"{artifacts: soon}", "{runs: -1}"} {
		if _, err := Parse([]byte("name: p\nretention: " + bad + "\nstages: [{name: s, jobs: [{name: j, steps: [{run: x}]}]}]\n")); err == nil {
			t.Errorf("retention %s accepted", bad)
		}
	}
}

func TestParams(t *testing.T) {
	base := "name: p\nstages: [{name: s, jobs: [{name: j, steps: [{run: x}]}]}]\n"
	for _, bad := range []string{
		`params: [{name: "bad-name"}]`,
		`params: [{name: A}, {name: A}]`,
		`params: [{name: A, type: number}]`,
		`params: [{name: A, type: choice}]`,
		`params: [{name: A, type: choice, options: [x, x]}]`,
		`params: [{name: A, type: choice, options: [x], default: y}]`,
		`params: [{name: A, options: [x]}]`,
		`params: [{name: A, type: boolean, default: yes}]`,
	} {
		if _, err := Parse([]byte(base + bad)); err == nil {
			t.Errorf("%s: want error", bad)
		}
	}

	s, err := Parse([]byte(base + `params:
  - {name: TARGET, type: choice, options: [staging, prod], default: staging}
  - {name: DRY, type: boolean}
  - {name: VERSION, required: true}
  - {name: NOTE}
`))
	if err != nil {
		t.Fatal(err)
	}
	if s.Params[2].Type != ParamString {
		t.Errorf("default type = %q", s.Params[2].Type)
	}
	env, err := s.ApplyParams(map[string]string{"VERSION": "1.0", "OTHER": "x"}, true)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"TARGET": "staging", "DRY": "false", "VERSION": "1.0", "OTHER": "x"}
	if !reflect.DeepEqual(env, want) {
		t.Errorf("env = %v", env)
	}
	for _, in := range []map[string]string{
		{"VERSION": ""},
		{"VERSION": "1", "TARGET": "dev"},
		{"VERSION": "1", "DRY": "1"},
	} {
		if _, err := s.ApplyParams(in, true); err == nil {
			t.Errorf("%v: want error", in)
		}
	}
	// 自动触发不强制必填。
	if _, err := s.ApplyParams(nil, false); err != nil {
		t.Errorf("automatic run: %v", err)
	}
}

func TestConcurrencyAndStatus(t *testing.T) {
	base := "name: p\nenv: {REGION: cn}\nparams: [{name: TARGET}]\nstages: [{name: s, jobs: [{name: j, steps: [{run: x}]}]}]\n"
	for _, bad := range []string{
		`concurrency: {group: "deploy-${UNKNOWN}"}`,
		`concurrency: {group: "` + strings.Repeat("x", 101) + `"}`,
		`source: {url: "https://github.com/o/r", report_status: true}`,
		`source: {url: "https://code.example.com/o/r", token_secret: T, report_status: true}`,
		`source: {url: "https://github.com/o/r", provider: bitbucket}`,
	} {
		if _, err := Parse([]byte(base + bad)); err == nil {
			t.Errorf("%s: want error", bad)
		}
	}
	s, err := Parse([]byte(base + `concurrency: {group: "deploy-${TARGET}-${REGION}"}
source: {url: "https://code.example.com/o/r", provider: gitlab, token_secret: T, report_status: true}
`))
	if err != nil {
		t.Fatal(err)
	}
	if g := s.ConcurrencyGroup("pid", map[string]string{"TARGET": "prod"}); g != "deploy-prod-cn" {
		t.Errorf("group = %q", g)
	}
	s.Concurrency.Group = ""
	if g := s.ConcurrencyGroup("pid", nil); g != "pipeline/pid" {
		t.Errorf("default group = %q", g)
	}
	s.Concurrency = nil
	if g := s.ConcurrencyGroup("pid", nil); g != "" {
		t.Errorf("no concurrency group = %q", g)
	}
}
