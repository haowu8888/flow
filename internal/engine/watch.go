package engine

import (
	"context"
	"sync"
)

// hub 是进程内的变更广播：watch 返回一个在下次 notify 同一 key 时关闭的 channel。
// 只覆盖本实例内发生的变更；多实例部署时订阅方还需定期轮询兜底。
type hub struct {
	mu sync.Mutex
	m  map[string]*watchers
}

type watchers struct {
	ch chan struct{}
	n  int
}

func (h *hub) watch(key string) (<-chan struct{}, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.m == nil {
		h.m = map[string]*watchers{}
	}
	w := h.m[key]
	if w == nil {
		w = &watchers{ch: make(chan struct{})}
		h.m[key] = w
	}
	w.n++
	var once sync.Once
	return w.ch, func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if w.n--; w.n == 0 && h.m[key] == w {
				delete(h.m, key)
			}
		})
	}
}

func (h *hub) notify(keys ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, k := range keys {
		if w := h.m[k]; w != nil {
			close(w.ch)
			delete(h.m, k)
		}
	}
}

func logKey(jobID string) string { return "log:" + jobID }
func runKey(runID string) string { return "run:" + runID }

// WatchLog 返回在该 Job 追加日志时关闭的 channel；用完须调用 release。
func (e *Engine) WatchLog(jobID string) (ch <-chan struct{}, release func()) {
	return e.hub.watch(logKey(jobID))
}

// WatchRun 返回在该 Run 或其任一 Job 状态变化、产物上传时关闭的 channel；用完须调用 release。
func (e *Engine) WatchRun(runID string) (ch <-chan struct{}, release func()) {
	return e.hub.watch(runKey(runID))
}

func (e *Engine) runChanged(runID string) { e.hub.notify(runKey(runID)) }

// appendLog 追加日志并唤醒订阅者。
func (e *Engine) appendLog(ctx context.Context, jobID string, data []byte) error {
	if err := e.store.AppendLog(ctx, jobID, data); err != nil {
		return err
	}
	e.hub.notify(logKey(jobID))
	return nil
}
