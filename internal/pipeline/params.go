package pipeline

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// 运行参数类型。
const (
	ParamString  = "string"
	ParamChoice  = "choice"
	ParamBoolean = "boolean"
)

// Param 声明一个运行参数：手动运行时在表单中填写，以同名环境变量注入所有 Job。
type Param struct {
	Name        string   `yaml:"name"`
	Type        string   `yaml:"type"` // string（默认）/ choice / boolean
	Default     string   `yaml:"default"`
	Options     []string `yaml:"options"` // choice 的可选值
	Description string   `yaml:"description"`
	Required    bool     `yaml:"required"` // 手动 / API 运行时必须有值（有默认值即满足）
}

func (s *Spec) validateParams() error {
	seen := make(map[string]bool)
	for i := range s.Params {
		p := &s.Params[i]
		if !ValidEnvName(p.Name) {
			return fmt.Errorf("params[%d]: %q is not a valid variable name", i, p.Name)
		}
		if seen[p.Name] {
			return fmt.Errorf("duplicate param %q", p.Name)
		}
		seen[p.Name] = true
		if p.Type == "" {
			p.Type = ParamString
		}
		switch p.Type {
		case ParamString:
			if len(p.Options) > 0 {
				return fmt.Errorf("param %q: options are only allowed for type choice", p.Name)
			}
		case ParamChoice:
			if len(p.Options) == 0 {
				return fmt.Errorf("param %q: choice needs at least one option", p.Name)
			}
			for k, o := range p.Options {
				if o == "" || slices.Contains(p.Options[:k], o) {
					return fmt.Errorf("param %q: options must be non-empty and unique", p.Name)
				}
			}
		case ParamBoolean:
			if len(p.Options) > 0 {
				return fmt.Errorf("param %q: options are only allowed for type choice", p.Name)
			}
		default:
			return fmt.Errorf("param %q: type must be one of string, choice, boolean", p.Name)
		}
		if p.Default != "" {
			if err := p.check(p.Default); err != nil {
				return fmt.Errorf("param %q default: %w", p.Name, err)
			}
		}
	}
	return nil
}

func (p *Param) check(v string) error {
	switch p.Type {
	case ParamChoice:
		if !slices.Contains(p.Options, v) {
			return fmt.Errorf("%q is not one of %s", v, strings.Join(p.Options, ", "))
		}
	case ParamBoolean:
		if v != "true" && v != "false" {
			return errors.New("must be true or false")
		}
	}
	return nil
}

// ApplyParams 返回补全默认值后的变量：未提供（或为空）的参数取默认值，布尔参数缺省为 false。
// 提供的值须符合类型；requireValues 为 true（手动 / API 运行）时必填参数不能为空。
// 其余变量原样保留。
func (s *Spec) ApplyParams(env map[string]string, requireValues bool) (map[string]string, error) {
	if len(s.Params) == 0 {
		return env, nil
	}
	out := maps.Clone(env)
	if out == nil {
		out = make(map[string]string)
	}
	for i := range s.Params {
		p := &s.Params[i]
		v := out[p.Name]
		if v == "" {
			v = p.Default
			if v == "" && p.Type == ParamBoolean {
				v = "false"
			}
		}
		if v == "" {
			if p.Required && requireValues {
				return nil, fmt.Errorf("param %s is required", p.Name)
			}
			continue
		}
		if err := p.check(v); err != nil {
			return nil, fmt.Errorf("param %s: %w", p.Name, err)
		}
		out[p.Name] = v
	}
	return out, nil
}
