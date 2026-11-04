// Package junit 解析 JUnit XML 测试报告（Maven Surefire、Gradle、pytest、go-junit-report、Jest 等通用格式）。
package junit

import (
	"encoding/xml"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"

	"flow/internal/model"
)

type suite struct {
	Name   string  `xml:"name,attr"`
	Suites []suite `xml:"testsuite"`
	Cases  []tcase `xml:"testcase"`
}

type tcase struct {
	Class   string   `xml:"classname,attr"`
	Name    string   `xml:"name,attr"`
	Time    string   `xml:"time,attr"`
	Failure *outcome `xml:"failure"`
	Error   *outcome `xml:"error"`
	Skipped *outcome `xml:"skipped"`
}

type outcome struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Text    string `xml:",chardata"`
}

// MaxMessage 是保留的失败信息长度上限（字节）。
const MaxMessage = 4 << 10

// Parse 读取一个 JUnit XML 文件，返回其中的全部用例。根元素可以是 testsuites 或 testsuite。
func Parse(r io.Reader) ([]model.TestCase, error) {
	dec := xml.NewDecoder(r)
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, errors.New("no <testsuite> or <testsuites> element")
			}
			return nil, err
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		var s suite
		if err := dec.DecodeElement(&s, &start); err != nil {
			return nil, err
		}
		switch start.Name.Local {
		case "testsuites", "testsuite":
		default:
			return nil, errors.New("root element must be <testsuites> or <testsuite>, got <" + start.Name.Local + ">")
		}
		var out []model.TestCase
		collect(&out, s, start.Name.Local == "testsuite")
		return out, nil
	}
}

func collect(out *[]model.TestCase, s suite, named bool) {
	for _, c := range s.Cases {
		tc := model.TestCase{Class: c.Class, Name: c.Name, Status: model.CasePassed}
		if named {
			tc.Suite = s.Name
		}
		tc.Time, _ = strconv.ParseFloat(strings.TrimSpace(c.Time), 64)
		switch {
		case c.Failure != nil:
			tc.Status, tc.Message = model.CaseFailed, message(c.Failure)
		case c.Error != nil:
			tc.Status, tc.Message = model.CaseFailed, message(c.Error)
		case c.Skipped != nil:
			tc.Status, tc.Message = model.CaseSkipped, strings.TrimSpace(c.Skipped.Message)
		}
		*out = append(*out, tc)
	}
	for _, sub := range s.Suites {
		collect(out, sub, true)
	}
}

func message(o *outcome) string {
	parts := []string{}
	if m := strings.TrimSpace(o.Message); m != "" {
		parts = append(parts, m)
	}
	if t := strings.TrimSpace(o.Text); t != "" && t != strings.TrimSpace(o.Message) {
		parts = append(parts, t)
	}
	msg := strings.Join(parts, "\n")
	if msg == "" {
		msg = o.Type
	}
	if len(msg) > MaxMessage {
		msg = msg[:MaxMessage] + "\n…"
	}
	return msg
}

// 报告中最多保留的用例：失败与跳过的全部保留（各有上限），通过的只保留最慢的若干个。
const (
	maxFailedCases  = 1000
	maxSkippedCases = 300
	slowestPassed   = 20
)

// Summarize 汇总用例，返回汇总与要保存的用例子集。
func Summarize(cases []model.TestCase) (model.TestSummary, []model.TestCase) {
	var s model.TestSummary
	var failed, skipped, passed []model.TestCase
	for _, c := range cases {
		s.Total++
		s.Duration += c.Time
		switch c.Status {
		case model.CaseFailed:
			s.Failed++
			failed = append(failed, c)
		case model.CaseSkipped:
			s.Skipped++
			skipped = append(skipped, c)
		default:
			s.Passed++
			passed = append(passed, c)
		}
	}
	slices.SortStableFunc(passed, func(a, b model.TestCase) int {
		switch {
		case a.Time > b.Time:
			return -1
		case a.Time < b.Time:
			return 1
		}
		return 0
	})
	keep := append(append(head(failed, maxFailedCases), head(skipped, maxSkippedCases)...), head(passed, slowestPassed)...)
	if keep == nil {
		keep = []model.TestCase{}
	}
	return s, keep
}

func head[T any](s []T, n int) []T { return s[:min(len(s), n)] }
