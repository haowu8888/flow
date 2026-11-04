package api

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"flow/internal/model"
	"flow/internal/protocol"
	"flow/internal/store"
)

type sseEvent struct{ event, id, data string }

// openSSE 发起 SSE 请求并在后台解析事件，测试结束或 ctx 取消时断开。
func openSSE(t *testing.T, ts *stack, path string, header http.Header) (<-chan sseEvent, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+ts.token)
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, resp.StatusCode
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content type = %q", ct)
	}
	ch := make(chan sseEvent, 64)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(nil, 8<<20)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev.event != "" {
					ch <- ev
				}
				ev = sseEvent{}
			case strings.HasPrefix(line, "event: "):
				ev.event = line[7:]
			case strings.HasPrefix(line, "id: "):
				ev.id = line[4:]
			case strings.HasPrefix(line, "data: "):
				ev.data = line[6:]
			}
		}
	}()
	return ch, http.StatusOK
}

func next(t *testing.T, ch <-chan sseEvent) (sseEvent, bool) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		return ev, ok
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for SSE event")
		return sseEvent{}, false
	}
}

func TestLogStream(t *testing.T) {
	ts := startStack(t, store.NewMemory())
	p := createPipeline(t, ts, `
name: sse
stages:
  - name: build
    jobs:
      - {name: build, steps: [{run: "echo first; sleep 0.5; echo 中文second"}]}
`)
	detail := post[protocol.RunDetail](t, ts, "/api/pipelines/"+p.ID+"/runs", `{}`, http.StatusCreated)
	jobID := detail.Jobs[0].ID

	ch, code := openSSE(t, ts, "/api/jobs/"+jobID+"/logs/stream", nil)
	if code != http.StatusOK {
		t.Fatalf("stream = %d", code)
	}
	var log strings.Builder
	var firstID, endStatus string
	for endStatus == "" {
		ev, ok := next(t, ch)
		if !ok {
			t.Fatal("stream closed before end event")
		}
		switch ev.event {
		case "log":
			data, err := base64.StdEncoding.DecodeString(ev.data)
			if err != nil {
				t.Fatal(err)
			}
			log.Write(data)
			if firstID == "" {
				firstID = ev.id
			}
		case "end":
			var end struct{ Status string }
			json.Unmarshal([]byte(ev.data), &end)
			endStatus = end.Status
		}
	}
	if endStatus != string(model.StatusSuccess) {
		t.Errorf("end status = %s", endStatus)
	}
	full := getText(t, ts, "/api/jobs/"+jobID+"/logs")
	if log.String() != full {
		t.Errorf("streamed log differs from stored log:\n%q\n%q", log.String(), full)
	}
	if !strings.Contains(full, "first") || !strings.Contains(full, "中文second") {
		t.Errorf("log = %q", full)
	}
	if _, ok := next(t, ch); ok {
		t.Error("stream not closed after end")
	}

	// 断线重连：Last-Event-ID 之后只推送剩余部分。
	ch, _ = openSSE(t, ts, "/api/jobs/"+jobID+"/logs/stream", http.Header{"Last-Event-ID": {firstID}})
	var rest strings.Builder
	for {
		ev, ok := next(t, ch)
		if !ok || ev.event == "end" {
			break
		}
		data, _ := base64.StdEncoding.DecodeString(ev.data)
		rest.Write(data)
	}
	var off int
	json.Unmarshal([]byte(firstID), &off)
	if rest.String() != full[off:] {
		t.Errorf("resumed log = %q, want %q", rest.String(), full[off:])
	}

	if _, code := openSSE(t, ts.as(""), "/api/jobs/"+jobID+"/logs/stream", nil); code != http.StatusUnauthorized {
		t.Errorf("anonymous stream = %d", code)
	}
}

func TestRunStream(t *testing.T) {
	ts := startStack(t, store.NewMemory())
	p := createPipeline(t, ts, `
name: sse-run
stages:
  - name: build
    jobs: [{name: build, steps: [{run: "sleep 0.3"}]}]
  - name: gate
    jobs: [{name: gate, type: approval}]
`)
	detail := post[protocol.RunDetail](t, ts, "/api/pipelines/"+p.ID+"/runs", `{}`, http.StatusCreated)
	ch, code := openSSE(t, ts, "/api/runs/"+detail.Run.ID+"/events", nil)
	if code != http.StatusOK {
		t.Fatalf("stream = %d", code)
	}
	// 依次观察到：构建运行 → 等待审批 → 审批后成功；每次变化都应在兜底轮询间隔内送达。
	var seen []string
	var approvedAt time.Time
	for {
		ev, ok := next(t, ch)
		if !ok {
			t.Fatal("stream closed")
		}
		if ev.event != "run" {
			continue
		}
		var d protocol.RunDetail
		if err := json.Unmarshal([]byte(ev.data), &d); err != nil {
			t.Fatal(err)
		}
		jobs := jobsByName(d)
		state := string(jobs["build"].Status) + "/" + string(jobs["gate"].Status)
		if len(seen) == 0 || seen[len(seen)-1] != state {
			seen = append(seen, state)
		}
		if jobs["gate"].Status == model.StatusWaiting && approvedAt.IsZero() {
			approvedAt = time.Now()
			postRaw(t, ts, "/api/jobs/"+jobs["gate"].ID+"/approve", `{"approve":true}`, http.StatusOK)
		}
		if d.Run.Status == model.StatusSuccess {
			// 进程内变更应立即推送，而不是等兜底轮询。
			if lag := time.Since(approvedAt); lag >= ssePollInterval {
				t.Errorf("approval pushed after %v", lag)
			}
			break
		}
	}
	if got := strings.Join(seen, " "); !strings.Contains(got, "running/pending") || !strings.Contains(got, "success/waiting") ||
		!strings.HasSuffix(got, "success/success") {
		t.Errorf("states = %s", got)
	}
}
