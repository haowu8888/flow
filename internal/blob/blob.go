// Package blob 存储构建产物等大文件：本地目录（单机）或 S3 兼容对象存储（MinIO / OSS / COS 等，多实例部署）。
package blob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var ErrNotFound = errors.New("blob not found")

type Store interface {
	// Put 写入 size 字节；size 未知时传 -1（S3 实现会先落盘以得到长度）。
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
}

func validKey(key string) bool {
	return key != "" && !strings.HasPrefix(key, "/") && !strings.Contains(key, "..") && !strings.Contains(key, "\\")
}

// Local 将对象存为 dir 下的文件，写入先写临时文件再改名，保证读到的总是完整对象。
type Local struct{ dir string }

func NewLocal(dir string) (*Local, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Local{dir: dir}, nil
}

func (l *Local) path(key string) (string, error) {
	if !validKey(key) {
		return "", errors.New("invalid blob key")
	}
	return filepath.Join(l.dir, filepath.FromSlash(key)), nil
}

func (l *Local) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), p)
}

func (l *Local) Open(_ context.Context, key string) (io.ReadCloser, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

func (l *Local) Delete(_ context.Context, key string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Memory 用于测试与内存模式。
type Memory struct {
	mu   sync.Mutex
	objs map[string][]byte
}

func NewMemory() *Memory { return &Memory{objs: make(map[string][]byte)} }

func (m *Memory) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[key] = data
	return nil
}

func (m *Memory) Open(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objs[key]
	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, key)
	return nil
}
