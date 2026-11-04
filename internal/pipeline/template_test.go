package pipeline

import (
	"errors"
	"strings"
	"testing"
)

const goBuild = `
description: 构建 Go 程序
inputs:
  GOOS: {default: linux}
  PKG: {required: true}
steps:
  - name: build
    run: GOOS=$GOOS go build ./$PKG
  - name: show
    env: {VERBOSE: "1"}
    run: ls
`

func TestStepTemplate(t *testing.T) {
	tmpl, err := ParseStepTemplate([]byte(goBuild))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := Parse([]byte(`
name: p
stages:
  - name: s
    jobs:
      - name: j
        matrix: {ARCH: [amd64, arm64]}
        steps:
          - run: echo before
          - name: compile
            uses: go-build
            with: {PKG: cmd/app}
            env: {CGO_ENABLED: "0"}
      - name: k
        steps: [{uses: go-build, with: {PKG: x, GOOS: darwin}}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(spec.UsedTemplates(), ","); got != "go-build" {
		t.Errorf("used = %s", got)
	}
	lookups := 0
	err = spec.ExpandTemplates(func(name string, _ int) (*StepTemplate, error) {
		lookups++
		if name != "go-build" {
			return nil, errors.New("not found")
		}
		return tmpl, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if lookups != 1 {
		t.Errorf("lookups = %d, want 1 (cached)", lookups)
	}
	j := spec.Stages[0].Jobs[0]
	if len(j.Steps) != 3 || j.Steps[1].Name != "compile / build" || j.Steps[2].Name != "compile / show" {
		t.Fatalf("steps = %+v", j.Steps)
	}
	env := j.Steps[1].Env
	if env["GOOS"] != "linux" || env["PKG"] != "cmd/app" || env["CGO_ENABLED"] != "0" || env["VERBOSE"] != "" {
		t.Errorf("env = %v", env)
	}
	if j.Steps[2].Env["VERBOSE"] != "1" {
		t.Errorf("template step env lost: %v", j.Steps[2].Env)
	}
	if k := spec.Stages[0].Jobs[2]; k.Steps[0].Name != "go-build / build" || k.Steps[0].Env["GOOS"] != "darwin" {
		t.Errorf("k = %+v", k.Steps)
	}
	if spec.Stages[0].Jobs[1].Steps[1].Env["PKG"] != "cmd/app" {
		t.Error("second matrix job not expanded")
	}

	bad := map[string]string{
		"missing input": `{uses: go-build}`,
		"unknown input": `{uses: go-build, with: {PKG: x, NOPE: y}}`,
		"no template":   `{uses: other}`,
	}
	for name, step := range bad {
		spec, err := Parse([]byte("name: p\nstages:\n  - name: s\n    jobs: [{name: j, steps: [" + step + "]}]\n"))
		if err != nil {
			t.Fatalf("%s: parse: %v", name, err)
		}
		if err := spec.ExpandTemplates(func(n string, _ int) (*StepTemplate, error) {
			if n != "go-build" {
				return nil, errors.New("not found")
			}
			return tmpl, nil
		}); err == nil {
			t.Errorf("%s: expanded", name)
		}
	}
}

func TestStepValidation(t *testing.T) {
	bad := []string{
		`{run: x, uses: t}`,
		`{uses: "Bad Name"}`,
		`{run: x, with: {A: b}}`,
		`{run: x, env: {"1A": b}}`,
		`{name: only}`,
	}
	for _, step := range bad {
		if _, err := Parse([]byte("name: p\nstages:\n  - name: s\n    jobs: [{name: j, steps: [" + step + "]}]\n")); err == nil {
			t.Errorf("step %s accepted", step)
		}
	}
	badTemplates := []string{
		`steps: []`,
		`steps: [{uses: other}]`,
		"inputs: {A: {required: true, default: x}}\nsteps: [{run: x}]",
		"inputs: {a-b: {}}\nsteps: [{run: x}]",
		`unknown: 1`,
	}
	for _, y := range badTemplates {
		if _, err := ParseStepTemplate([]byte(y)); err == nil {
			t.Errorf("template %q accepted", y)
		}
	}
}

func TestParseUses(t *testing.T) {
	for in, want := range map[string]struct {
		name    string
		version int
		ok      bool
	}{
		"go-build":    {"go-build", 0, true},
		"go-build@3":  {"go-build", 3, true},
		"go-build@0":  {"go-build", 0, false},
		"go-build@03": {"go-build", 0, false},
		"go-build@x":  {"go-build", 0, false},
		"Go@1":        {"Go", 1, false},
	} {
		name, v, ok := ParseUses(in)
		if name != want.name || v != want.version || ok != want.ok {
			t.Errorf("ParseUses(%q) = %q, %d, %v", in, name, v, ok)
		}
	}
}
