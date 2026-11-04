package engine

import (
	"testing"

	"flow/internal/pipeline"
)

// 内置模板都能解析，且分类、名称齐全。
func TestBuiltinTemplatesParse(t *testing.T) {
	for _, tpl := range builtinTemplates() {
		if tpl.Name == "" || tpl.Category == "" || tpl.Description == "" {
			t.Errorf("%s: missing metadata", tpl.ID)
		}
		if _, err := pipeline.Parse([]byte(tpl.YAML)); err != nil {
			t.Errorf("%s: %v", tpl.ID, err)
		}
	}
}
