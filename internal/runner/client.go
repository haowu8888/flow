package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"flow/internal/model"
	"flow/internal/protocol"
)

// ErrLeaseLost 表示 Server 已不再认为该 Runner 持有此 Job（超时被回收或已结束）。
var ErrLeaseLost = errors.New("lease lost")

type Client struct {
	base   string
	token  string
	http   *http.Client
	upload *http.Client // 产物传输可能较久，单独设置超时
}

func NewClient(baseURL, token string) *Client {
	return &Client{
		base: strings.TrimRight(baseURL, "/"), token: token,
		http:   &http.Client{Timeout: 30 * time.Second},
		upload: &http.Client{Timeout: 2 * time.Hour},
	}
}

func (c *Client) Claim(ctx context.Context, runnerID string, labels []string, info *model.NodeInfo) (*protocol.ClaimResponse, error) {
	var resp protocol.ClaimResponse
	code, err := c.postJSON(ctx, "/api/runner/claim", protocol.ClaimRequest{RunnerID: runnerID, Labels: labels, Info: info}, &resp)
	if err != nil || code == http.StatusNoContent {
		return nil, err
	}
	if resp.Job == nil {
		return nil, errors.New("claim response without job")
	}
	return &resp, nil
}

// UploadArtifact 流式上传 tar.gz 产物归档。
func (c *Client) UploadArtifact(ctx context.Context, jobID, runnerID string, body io.Reader) error {
	path := jobPath(jobID, "artifacts") + "?runner_id=" + url.QueryEscape(runnerID)
	_, err := c.doWith(ctx, c.upload, http.MethodPost, path, body, "application/gzip", nil)
	return err
}

// DownloadArtifact 下载产物归档，调用方负责关闭。
func (c *Client) DownloadArtifact(ctx context.Context, id string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/runner/artifacts/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.upload.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
		return nil, fmt.Errorf("download artifact %s: %s: %s", id, resp.Status, bytes.TrimSpace(msg))
	}
	return resp.Body, nil
}

func (c *Client) Heartbeat(ctx context.Context, jobID, runnerID string) (cancel bool, err error) {
	var resp protocol.HeartbeatResponse
	_, err = c.postJSON(ctx, jobPath(jobID, "heartbeat"), protocol.HeartbeatRequest{RunnerID: runnerID}, &resp)
	return resp.Cancel, err
}

func (c *Client) AppendLog(ctx context.Context, jobID, runnerID string, data []byte) error {
	path := jobPath(jobID, "logs") + "?runner_id=" + url.QueryEscape(runnerID)
	_, err := c.do(ctx, path, bytes.NewReader(data), "text/plain", nil)
	return err
}

func (c *Client) ReportStep(ctx context.Context, jobID string, req protocol.StepRequest) error {
	_, err := c.postJSON(ctx, jobPath(jobID, "steps"), req, nil)
	return err
}

func (c *Client) ReportHosts(ctx context.Context, jobID string, req protocol.HostsRequest) error {
	_, err := c.postJSON(ctx, jobPath(jobID, "hosts"), req, nil)
	return err
}

func (c *Client) UploadTestReport(ctx context.Context, jobID string, req protocol.TestReportRequest) error {
	_, err := c.postJSON(ctx, jobPath(jobID, "tests"), req, nil)
	return err
}

func (c *Client) ReportCommit(ctx context.Context, jobID, runnerID, commit string) error {
	_, err := c.postJSON(ctx, jobPath(jobID, "commit"), protocol.CommitRequest{RunnerID: runnerID, Commit: commit}, nil)
	return err
}

func (c *Client) Complete(ctx context.Context, jobID string, req protocol.CompleteRequest) error {
	_, err := c.postJSON(ctx, jobPath(jobID, "complete"), req, nil)
	return err
}

func jobPath(jobID, action string) string {
	return "/api/runner/jobs/" + url.PathEscape(jobID) + "/" + action
}

func (c *Client) postJSON(ctx context.Context, path string, in, out any) (int, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return 0, err
	}
	return c.do(ctx, path, bytes.NewReader(body), "application/json", out)
}

func (c *Client) do(ctx context.Context, path string, body io.Reader, contentType string, out any) (int, error) {
	return c.doWith(ctx, c.http, http.MethodPost, path, body, contentType, out)
}

func (c *Client) doWith(ctx context.Context, hc *http.Client, method, path string, body io.Reader, contentType string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", contentType)
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusConflict:
		return resp.StatusCode, ErrLeaseLost
	case resp.StatusCode >= 300:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return resp.StatusCode, fmt.Errorf("%s %s: %s: %s", req.Method, path, resp.Status, bytes.TrimSpace(msg))
	case out != nil && resp.StatusCode != http.StatusNoContent:
		return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode, nil
}

// ---- 主机 Agent 与 Agent 主机任务 ----

func (c *Client) CreateHostTask(ctx context.Context, jobID, runnerID string, in protocol.HostTaskRequest) (*model.HostTask, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var t model.HostTask
	path := jobPath(jobID, "host-tasks") + "?runner_id=" + url.QueryEscape(runnerID)
	_, err = c.doWith(ctx, c.upload, http.MethodPost, path, bytes.NewReader(body), "application/json", &t)
	return &t, err
}

func (c *Client) HostTaskProgress(ctx context.Context, taskID, runnerID string, offset int, cancel bool) (*protocol.HostTaskProgress, error) {
	var p protocol.HostTaskProgress
	path := fmt.Sprintf("/api/runner/host-tasks/%s?runner_id=%s&offset=%d", url.PathEscape(taskID), url.QueryEscape(runnerID), offset)
	if cancel {
		path += "&cancel=1"
	}
	_, err := c.doWith(ctx, c.http, http.MethodGet, path, nil, "", &p)
	return &p, err
}

func (c *Client) AgentClaim(ctx context.Context, info *model.NodeInfo) (*model.HostTask, error) {
	var t model.HostTask
	code, err := c.postJSON(ctx, "/api/runner/agent/claim", protocol.ClaimRequest{Info: info}, &t)
	if err != nil || code == http.StatusNoContent {
		return nil, err
	}
	return &t, nil
}

func (c *Client) AgentLog(ctx context.Context, taskID string, data []byte) (cancel bool, err error) {
	var resp protocol.HeartbeatResponse
	_, err = c.do(ctx, "/api/runner/agent/tasks/"+url.PathEscape(taskID)+"/log", bytes.NewReader(data), "application/octet-stream", &resp)
	return resp.Cancel, err
}

func (c *Client) AgentFinish(ctx context.Context, taskID string, exitCode int, errMsg string) error {
	_, err := c.postJSON(ctx, "/api/runner/agent/tasks/"+url.PathEscape(taskID)+"/finish", protocol.CompleteRequest{ExitCode: exitCode, Error: errMsg}, nil)
	return err
}

func (c *Client) AgentUpload(ctx context.Context, taskID string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/runner/agent/tasks/"+url.PathEscape(taskID)+"/upload", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.upload.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
		return nil, fmt.Errorf("download upload: %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	return resp.Body, nil
}
