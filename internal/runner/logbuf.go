package runner

import (
	"bytes"
	"context"
	"log"
	"sync"
	"time"
)

const maxLogUpload = 1 << 20

// logUploader 缓冲 Step 输出并定时批量上传；上传失败时保留缓冲，下次重试，保证顺序不丢。
type logUploader struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	flushMu sync.Mutex
	upload  func(ctx context.Context, data []byte) error
	done    chan struct{}
	wg      sync.WaitGroup
}

func newLogUploader(upload func(context.Context, []byte) error, interval time.Duration) *logUploader {
	u := &logUploader{upload: upload, done: make(chan struct{})}
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				u.flush()
			case <-u.done:
				return
			}
		}
	}()
	return u
}

func (u *logUploader) Write(p []byte) (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.buf.Write(p)
}

func (u *logUploader) flush() error {
	u.flushMu.Lock()
	defer u.flushMu.Unlock()
	for {
		u.mu.Lock()
		data := u.buf.Bytes()
		if len(data) > maxLogUpload {
			data = data[:maxLogUpload]
		}
		data = bytes.Clone(data)
		u.mu.Unlock()
		if len(data) == 0 {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := u.upload(ctx, data)
		cancel()
		if err != nil {
			log.Printf("upload log: %v", err)
			return err
		}
		u.mu.Lock()
		u.buf.Next(len(data))
		u.mu.Unlock()
	}
}

// Close 停止定时上传并尽力刷出剩余日志。
func (u *logUploader) Close() {
	close(u.done)
	u.wg.Wait()
	for i := 0; i < 3 && u.flush() != nil; i++ {
		time.Sleep(time.Second)
	}
}
