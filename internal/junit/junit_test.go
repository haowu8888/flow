package junit

import (
	"strings"
	"testing"

	"flow/internal/model"
)

func TestParse(t *testing.T) {
	surefire := `<?xml version="1.0" encoding="UTF-8"?>
<testsuite name="com.acme.OrderTest" tests="3" failures="1" skipped="1" time="0.5">
  <testcase classname="com.acme.OrderTest" name="creates" time="0.1"/>
  <testcase classname="com.acme.OrderTest" name="rejects" time="0.3">
    <failure message="expected 2 but was 3" type="AssertionError">at OrderTest.java:42</failure>
  </testcase>
  <testcase classname="com.acme.OrderTest" name="later" time="0"><skipped message="todo"/></testcase>
</testsuite>`
	cases, err := Parse(strings.NewReader(surefire))
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 3 || cases[1].Status != model.CaseFailed || !strings.Contains(cases[1].Message, "expected 2 but was 3\nat OrderTest.java:42") ||
		cases[2].Status != model.CaseSkipped || cases[0].Suite != "com.acme.OrderTest" || cases[1].Time != 0.3 {
		t.Fatalf("cases = %+v", cases)
	}

	nested := `<testsuites><testsuite name="a"><testcase name="x"><error message="boom"/></testcase></testsuite>
<testsuite name="b"><testsuite name="b.inner"><testcase name="y" time="1.5"/></testsuite></testsuite></testsuites>`
	cases, err = Parse(strings.NewReader(nested))
	if err != nil || len(cases) != 2 || cases[0].Message != "boom" || cases[1].Suite != "b.inner" {
		t.Fatalf("nested = %+v, %v", cases, err)
	}
	s, keep := Summarize(append(cases, model.TestCase{Name: "z", Status: model.CaseSkipped}))
	if s.Total != 3 || s.Failed != 1 || s.Passed != 1 || s.Skipped != 1 || s.Duration != 1.5 || len(keep) != 3 || keep[0].Name != "x" {
		t.Errorf("summary = %+v keep %+v", s, keep)
	}
	if _, err := Parse(strings.NewReader("<html/>")); err == nil {
		t.Error("non-junit accepted")
	}
}
