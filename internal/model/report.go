package model

import "time"

// 测试用例结果。
const (
	CasePassed  = "passed"
	CaseFailed  = "failed" // 含 JUnit 的 failure 与 error
	CaseSkipped = "skipped"
)

// TestCase 是一个测试用例的结果。
type TestCase struct {
	Suite   string  `json:"suite,omitempty"`
	Class   string  `json:"class,omitempty"`
	Name    string  `json:"name"`
	Status  string  `json:"status"`
	Time    float64 `json:"time,omitempty"`    // 秒
	Message string  `json:"message,omitempty"` // 失败信息（截断）
}

// TestSummary 是测试数量汇总。
type TestSummary struct {
	Total    int     `json:"total"`
	Passed   int     `json:"passed"`
	Failed   int     `json:"failed"`
	Skipped  int     `json:"skipped"`
	Duration float64 `json:"duration"` // 秒，各用例耗时之和
}

// TestReport 是一个 Job 的测试报告（由 Runner 解析 JUnit XML 后上传，重试时覆盖）。
type TestReport struct {
	JobID    string `json:"job_id"`
	RunID    string `json:"run_id"`
	TenantID string `json:"-"`
	JobName  string `json:"job_name"`
	TestSummary
	// Cases 包含全部失败与跳过的用例，以及耗时最长的若干通过用例。
	Cases     []TestCase `json:"cases"`
	CreatedAt time.Time  `json:"created_at"`
}
