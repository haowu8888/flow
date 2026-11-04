package pipeline

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const maxTemplateSteps = 50

var templateNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidTemplateName 要求模板名为小写字母、数字、. _ -，以字母或数字开头，最长 64。
func ValidTemplateName(name string) bool { return templateNameRe.MatchString(name) }

// ParseUses 解析步骤的 uses：name 引用最新版本（version 为 0），name@N 锁定第 N 版。
func ParseUses(uses string) (name string, version int, ok bool) {
	name, v, pinned := strings.Cut(uses, "@")
	if pinned {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || strconv.Itoa(n) != v {
			return name, 0, false
		}
		version = n
	}
	return name, version, ValidTemplateName(name)
}

// StepTemplate 是可复用的步骤组（租户级），流水线中以 uses 引用、with 传入参数：
//
//	inputs:
//	  GOOS: {default: linux}
//	  PKG: {required: true, description: 要构建的包}
//	steps:
//	  - name: build
//	    run: GOOS=$GOOS go build ./$PKG
//
// 输入注入为模板各步骤的环境变量；模板中的步骤不能再引用其他模板。
type StepTemplate struct {
	Description string           `yaml:"description"`
	Inputs      map[string]Input `yaml:"inputs"`
	Steps       []Step           `yaml:"steps"`
}

type Input struct {
	Description string  `yaml:"description"`
	Default     *string `yaml:"default"`
	Required    bool    `yaml:"required"`
}

// ParseStepTemplate 解析并校验步骤模板。
func ParseStepTemplate(data []byte) (*StepTemplate, error) {
	var t StepTemplate
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	if len(t.Steps) == 0 {
		return nil, errors.New("at least one step is required")
	}
	if len(t.Steps) > maxTemplateSteps {
		return nil, fmt.Errorf("a template may contain at most %d steps", maxTemplateSteps)
	}
	for name, in := range t.Inputs {
		if !ValidEnvName(name) {
			return nil, fmt.Errorf("input %q is not a valid variable name", name)
		}
		if in.Required && in.Default != nil {
			return nil, fmt.Errorf("input %q: required inputs cannot have a default", name)
		}
	}
	for k := range t.Steps {
		if err := t.Steps[k].validate(k, false); err != nil {
			return nil, err
		}
	}
	return &t, nil
}

// TemplateLookup 按名称与版本（0 为最新）查找步骤模板，不存在时返回错误。
type TemplateLookup func(name string, version int) (*StepTemplate, error)

// HostGroups 返回部署任务引用的主机组名（去重排序）。
func (s *Spec) HostGroups() []string {
	seen := map[string]bool{}
	for _, st := range s.Stages {
		for _, j := range st.Jobs {
			if j.Deploy != nil {
				seen[j.Deploy.Hosts] = true
			}
		}
	}
	names := slices.Collect(maps.Keys(seen))
	sort.Strings(names)
	return names
}

// UsedTemplates 返回流水线引用的步骤模板名（去重排序，不含版本）。
func (s *Spec) UsedTemplates() []string {
	names := slices.Collect(maps.Keys(s.TemplateRefs()))
	sort.Strings(names)
	return names
}

// TemplateRefs 返回每个被引用的步骤模板所用的版本（升序去重，0 表示最新版本）。
func (s *Spec) TemplateRefs() map[string][]int {
	out := map[string][]int{}
	for _, st := range s.Stages {
		for _, j := range st.Jobs {
			for _, step := range j.Steps {
				if name, v, ok := ParseUses(step.Uses); ok && !slices.Contains(out[name], v) {
					out[name] = append(out[name], v)
				}
			}
		}
	}
	for _, vs := range out {
		slices.Sort(vs)
	}
	return out
}

// ExpandTemplates 把 uses 步骤替换为模板中的步骤：
// 步骤名为「引用步骤名 / 模板步骤名」，环境变量依次为模板输入（默认值 < with）、模板步骤自身的 env、引用步骤的 env。
func (s *Spec) ExpandTemplates(lookup TemplateLookup) error {
	cache := map[string]*StepTemplate{}
	for si := range s.Stages {
		for ji := range s.Stages[si].Jobs {
			j := &s.Stages[si].Jobs[ji]
			var steps []Step
			for _, step := range j.Steps {
				if step.Uses == "" {
					steps = append(steps, step)
					continue
				}
				t, ok := cache[step.Uses]
				if !ok {
					name, version, _ := ParseUses(step.Uses)
					var err error
					if t, err = lookup(name, version); err != nil {
						return fmt.Errorf("job %q step %q: %w", j.Name, step.Name, err)
					}
					cache[step.Uses] = t
				}
				expanded, err := t.instantiate(step)
				if err != nil {
					return fmt.Errorf("job %q step %q: template %q: %w", j.Name, step.Name, step.Uses, err)
				}
				steps = append(steps, expanded...)
			}
			j.Steps = steps
		}
	}
	return nil
}

func (t *StepTemplate) instantiate(ref Step) ([]Step, error) {
	inputs := map[string]string{}
	var missing []string
	for name, in := range t.Inputs {
		switch v, ok := ref.With[name]; {
		case ok:
			inputs[name] = v
		case in.Default != nil:
			inputs[name] = *in.Default
		case in.Required:
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("missing required input(s) %s", strings.Join(missing, ", "))
	}
	for name := range ref.With {
		if _, ok := t.Inputs[name]; !ok {
			return nil, fmt.Errorf("unknown input %q", name)
		}
	}
	out := make([]Step, len(t.Steps))
	for i, ts := range t.Steps {
		env := maps.Clone(inputs)
		maps.Copy(env, ts.Env)
		maps.Copy(env, ref.Env)
		if len(env) == 0 {
			env = nil
		}
		out[i] = Step{Name: ref.Name + " / " + ts.Name, Run: ts.Run, Env: env}
	}
	return out, nil
}
