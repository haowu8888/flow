package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"flow/internal/auth"
	"flow/internal/model"
	"flow/internal/protocol"
)

// SSE 推送参数。进程内变更会立即唤醒推送；pollInterval 兜底其他实例上发生的变更。
const (
	ssePollInterval     = 2 * time.Second
	sseIdlePollInterval = 15 * time.Second // 已结束的 Run 很少再变化（重试除外）
	sseKeepAlive        = 15 * time.Second
	// sseMaxDuration 到期后服务端主动断开，浏览器自动重连并重新鉴权（会话被注销后不再继续推送）。
	sseMaxDuration = 10 * time.Minute
	sseRetryMillis = 3000
	// logEndGrace：Job 结束后再等这么久才结束日志流，以收齐结束后追加的提示（如审批结论）。
	logEndGrace = 2 * time.Second
)

type sseStream struct {
	w   http.ResponseWriter
	rc  *http.ResponseController
	ctx context.Context
}

// startSSE 写出 SSE 响应头；返回的 ctx 在客户端断开、服务关闭或到达最长时长时结束。
func (s *server) startSSE(w http.ResponseWriter, r *http.Request) (*sseStream, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(r.Context(), sseMaxDuration)
	if s.cfg.Done != nil {
		go func() {
			select {
			case <-s.cfg.Done:
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{}) // 不受服务端写超时限制
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // 关闭 Nginx 缓冲
	w.WriteHeader(http.StatusOK)
	st := &sseStream{w: w, rc: rc, ctx: ctx}
	fmt.Fprintf(w, "retry: %d\n\n", sseRetryMillis)
	_ = rc.Flush()
	return st, cancel
}

// send 写出一条事件；data 不得包含换行（调用方传 JSON 或 base64）。
func (st *sseStream) send(event, id string, data []byte) error {
	var b bytes.Buffer
	if id != "" {
		fmt.Fprintf(&b, "id: %s\n", id)
	}
	fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", event, data)
	if _, err := st.w.Write(b.Bytes()); err != nil {
		return err
	}
	return st.rc.Flush()
}

func (st *sseStream) ping() error {
	if _, err := st.w.Write([]byte(": ping\n\n")); err != nil {
		return err
	}
	return st.rc.Flush()
}

// wait 阻塞到任一 channel 关闭、兜底轮询到期或流结束；需要发送保活时先发送。返回 false 表示应结束。
func (st *sseStream) wait(poll time.Duration, lastWrite *time.Time, chans ...<-chan struct{}) bool {
	keep := time.Until(lastWrite.Add(sseKeepAlive))
	timer := time.NewTimer(min(poll, max(keep, 0)))
	defer timer.Stop()
	var a, b <-chan struct{}
	if len(chans) > 0 {
		a = chans[0]
	}
	if len(chans) > 1 {
		b = chans[1]
	}
	select {
	case <-st.ctx.Done():
		return false
	case <-a:
	case <-b:
	case <-timer.C:
		if time.Since(*lastWrite) >= sseKeepAlive {
			if st.ping() != nil {
				return false
			}
			*lastWrite = time.Now()
		}
	}
	return true
}

// streamLog 以 SSE 推送 Job 日志：
//
//	event: log  id: <下一偏移>  data: <base64 字节>
//	event: end  data: {"status":"success"}
//
// 断线重连时浏览器带上 Last-Event-ID 从该偏移续传；也可用 ?offset= 指定起点。
func (s *server) streamLog(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	jobID := r.PathValue("id")
	if _, err := s.jobOf(r.Context(), jobID, p, model.PermView); err != nil {
		respond(w, 0, nil, err)
		return
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if id := r.Header.Get("Last-Event-ID"); id != "" {
		offset, _ = strconv.Atoi(id)
	}
	offset = max(offset, 0)

	st, cancel := s.startSSE(w, r)
	defer cancel()
	lastWrite := time.Now()
	for {
		logCh, releaseLog := s.eng.WatchLog(jobID)
		job, err := s.eng.Job(st.ctx, jobID)
		if err != nil {
			releaseLog()
			return
		}
		runCh, releaseRun := s.eng.WatchRun(job.RunID)
		// 先读状态再读日志：若此时已结束，读到的日志就是结束前写入的全部内容。
		data, err := s.eng.ReadLog(st.ctx, jobID, offset)
		ok := err == nil
		switch {
		case !ok:
		case len(data) > 0:
			offset += len(data)
			enc := make([]byte, base64.StdEncoding.EncodedLen(len(data)))
			base64.StdEncoding.Encode(enc, data)
			ok = st.send("log", strconv.Itoa(offset), enc) == nil
			lastWrite = time.Now()
		case job.Status.Terminal() && (job.FinishedAt == nil || time.Since(*job.FinishedAt) >= logEndGrace):
			end, _ := json.Marshal(map[string]model.Status{"status": job.Status})
			_ = st.send("end", strconv.Itoa(offset), end)
			ok = false
		default:
			poll := ssePollInterval
			if job.Status.Terminal() {
				poll = logEndGrace / 4
			}
			ok = st.wait(poll, &lastWrite, logCh, runCh)
		}
		releaseLog()
		releaseRun()
		if !ok {
			return
		}
	}
}

// streamRun 以 SSE 推送 Run 详情（与 GET /api/runs/{id} 相同的 JSON），内容变化时才发送：
//
//	event: run  data: {"run":{...},"jobs":[...]}
//
// Run 结束后流仍保持（重试会让它重新开始），只是降低兜底轮询频率。
func (s *server) streamRun(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	runID := r.PathValue("id")
	if _, err := s.runOf(r.Context(), runID, p, model.PermView); err != nil {
		respond(w, 0, nil, err)
		return
	}
	st, cancel := s.startSSE(w, r)
	defer cancel()
	lastWrite := time.Now()
	var last []byte
	for {
		ch, release := s.eng.WatchRun(runID)
		run, err := s.eng.Run(st.ctx, runID)
		var jobs []*model.Job
		if err == nil {
			jobs, err = s.eng.Jobs(st.ctx, runID)
		}
		ok := err == nil
		if ok {
			data, _ := json.Marshal(protocol.RunDetail{Run: run, Jobs: jobs})
			if !bytes.Equal(data, last) {
				last = data
				ok = st.send("run", "", data) == nil
				lastWrite = time.Now()
			}
		}
		if ok {
			poll := ssePollInterval
			if run.Status.Terminal() {
				poll = sseIdlePollInterval
			}
			ok = st.wait(poll, &lastWrite, ch)
		}
		release()
		if !ok {
			return
		}
	}
}
