package pipeline

import (
	"os"
	"testing"
)

func TestExamplesParse(t *testing.T) {
	for _, f := range []string{"../../examples/demo.yml", "../../examples/full.yml"} {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(data); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}
