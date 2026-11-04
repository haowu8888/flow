package secret

import (
	"bytes"
	"io"
	"sort"
	"strings"
	"sync"
)

// MinMaskLen 以下的值不脱敏，避免把日志里常见的短字符串全部替换掉。
const MinMaskLen = 4

const maxPending = 64 << 10

// Masker 包装 io.Writer，按行把密钥值替换为 ***。多行密钥逐行脱敏。
// 输出在遇到换行时写出；单行超过 64KB 时强制写出（可能漏掉恰好跨越边界的值）。
type Masker struct {
	mu      sync.Mutex
	w       io.Writer
	values  []string
	pending []byte
}

func NewMasker(w io.Writer, secrets []string) *Masker {
	seen := make(map[string]bool)
	var values []string
	for _, s := range secrets {
		for _, line := range strings.Split(s, "\n") {
			line = strings.TrimRight(line, "\r")
			if len(strings.TrimSpace(line)) >= MinMaskLen && !seen[line] {
				seen[line] = true
				values = append(values, line)
			}
		}
	}
	// 先替换较长的值，避免短值是长值的子串时残留部分明文。
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	return &Masker{w: w, values: values}
}

func (m *Masker) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.values) == 0 {
		return m.w.Write(p)
	}
	m.pending = append(m.pending, p...)
	cut := bytes.LastIndexByte(m.pending, '\n') + 1
	if cut == 0 && len(m.pending) > maxPending {
		cut = len(m.pending)
	}
	if cut > 0 {
		if _, err := m.w.Write(m.mask(m.pending[:cut])); err != nil {
			return 0, err
		}
		m.pending = append(m.pending[:0], m.pending[cut:]...)
	}
	return len(p), nil
}

// Flush 写出不以换行结尾的剩余内容。
func (m *Masker) Flush() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.pending) == 0 {
		return nil
	}
	_, err := m.w.Write(m.mask(m.pending))
	m.pending = m.pending[:0]
	return err
}

func (m *Masker) mask(b []byte) []byte {
	for _, v := range m.values {
		b = bytes.ReplaceAll(b, []byte(v), []byte("***"))
	}
	return b
}
