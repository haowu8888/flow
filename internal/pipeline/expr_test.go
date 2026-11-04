package pipeline

import "testing"

func TestCond(t *testing.T) {
	vars := map[string]string{"FLOW_BRANCH": "release/1.2", "TARGET": "prod", "FORCE": "false", "N": "3"}
	cases := map[string]bool{
		"":                                       true,
		"FLOW_BRANCH == 'main'":                  false,
		"FLOW_BRANCH =~ '^release/'":             true,
		"FLOW_BRANCH !~ '^release/'":             false,
		"${TARGET} == \"prod\" && !FORCE":        true,
		"TARGET == 'test' || (N == 3 && TARGET)": true,
		"FORCE":                                  false,
		"!MISSING":                               true,
		"MISSING == ''":                          true,
		"TARGET != 'prod'":                       false,
	}
	for src, want := range cases {
		c, err := ParseCond(src)
		if err != nil {
			t.Fatalf("%q: %v", src, err)
		}
		if got, err := c.Eval(vars); err != nil || got != want {
			t.Errorf("%q = %v, %v; want %v", src, got, err, want)
		}
	}
	for _, bad := range []string{"A ==", "(A", "A =~ B", "A =~ '['", "'x", "A = B", "A && && B"} {
		if _, err := ParseCond(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}
