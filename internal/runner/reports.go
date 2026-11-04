package runner

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"flow/internal/junit"
	"flow/internal/model"
	"flow/internal/protocol"
)

// 上传请求体不超过 Server 的 1 MiB 限制：失败信息总量超出预算后只保留开头。
const (
	reportMessageBudget = 512 << 10
	truncatedMessage    = 300
)

// uploadReports 解析 Job 声明的 JUnit 报告并上传；步骤失败时同样执行（失败的测试正是要看的）。
// 只记录日志、不影响 Job 结果。
func (r *Runner) uploadReports(job *model.Job, dir string, out io.Writer) {
	if len(job.Reports) == 0 {
		return
	}
	paths, err := collectPaths(dir, job.Reports)
	if err != nil {
		fmt.Fprintf(out, "==> test report: %v\n", err)
		return
	}
	var cases []model.TestCase
	files := 0
	for _, p := range paths {
		if !strings.EqualFold(filepath.Ext(p), ".xml") {
			continue
		}
		f, err := os.Open(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil {
			continue // 目录等
		}
		cs, err := junit.Parse(f)
		f.Close()
		if err != nil {
			fmt.Fprintf(out, "==> test report: skip %s: %v\n", p, err)
			continue
		}
		files++
		cases = append(cases, cs...)
	}
	if files == 0 {
		fmt.Fprintf(out, "==> test report: no JUnit XML matched %s\n", strings.Join(job.Reports, ", "))
		return
	}
	sum, keep := junit.Summarize(cases)
	budget := reportMessageBudget
	for i := range keep {
		if m := keep[i].Message; len(m) > budget {
			keep[i].Message = m[:min(len(m), truncatedMessage)]
		}
		budget -= len(keep[i].Message)
	}
	fmt.Fprintf(out, "==> test report: %d tests, %d passed, %d failed, %d skipped (%d file(s))\n",
		sum.Total, sum.Passed, sum.Failed, sum.Skipped, files)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req := protocol.TestReportRequest{RunnerID: job.RunnerID, TestSummary: sum, Cases: keep}
	if err := r.client.UploadTestReport(ctx, job.ID, req); err != nil {
		log.Printf("job %s upload test report: %v", job.ID, err)
		fmt.Fprintf(out, "==> test report: upload failed: %v\n", err)
	}
}
