// Package sshx 封装部署用到的 SSH 操作：连接（校验或记录主机公钥）、执行脚本、以 tar 流上传文件。
package sshx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Target 是一台主机的连接信息。
type Target struct {
	Address    string
	Port       int
	User       string
	Password   string
	PrivateKey string
	// HostKey 为已信任的主机公钥（authorized_keys 格式）；为空时接受任意公钥（首次连接），由调用方记录 Client.HostKey。
	HostKey string
	// Jump 非空时先连接跳板机，再经其转发连接目标（等同 ssh -J）。
	Jump *Target
}

// ErrHostKeyMismatch 表示主机公钥与已信任的不一致（主机重装，或存在中间人）。
var ErrHostKeyMismatch = errors.New("host key mismatch")

// Client 是已建立的连接。
type Client struct {
	*ssh.Client
	HostKey string // 连接时主机出示的公钥（authorized_keys 格式，不含换行）
	jump    *Client
}

// Close 关闭连接（含跳板机连接）。
func (c *Client) Close() error {
	err := c.Client.Close()
	if c.jump != nil {
		c.jump.Close()
	}
	return err
}

const dialTimeout = 10 * time.Second

// Dial 建立连接。
func Dial(ctx context.Context, t Target) (*Client, error) {
	var auth []ssh.AuthMethod
	if t.PrivateKey != "" {
		signer, err := ssh.ParsePrivateKey([]byte(t.PrivateKey))
		if err != nil {
			return nil, fmt.Errorf("parse private key: %w", err)
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if t.Password != "" {
		auth = append(auth, ssh.Password(t.Password), ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
			ans := make([]string, len(qs))
			for i := range ans {
				ans[i] = t.Password
			}
			return ans, nil
		}))
	}
	if len(auth) == 0 {
		return nil, errors.New("no credential (private key or password)")
	}
	var trusted ssh.PublicKey
	if t.HostKey != "" {
		k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(t.HostKey))
		if err != nil {
			return nil, fmt.Errorf("parse trusted host key: %w", err)
		}
		trusted = k
	}
	var seen string
	cfg := &ssh.ClientConfig{
		User: t.User,
		Auth: auth,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			seen = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
			if trusted != nil && !bytes.Equal(trusted.Marshal(), key.Marshal()) {
				return fmt.Errorf("%w: trusted %s, got %s", ErrHostKeyMismatch, ssh.FingerprintSHA256(trusted), ssh.FingerprintSHA256(key))
			}
			return nil
		},
		Timeout: dialTimeout,
	}
	port := t.Port
	if port == 0 {
		port = 22
	}
	addr := net.JoinHostPort(t.Address, strconv.Itoa(port))
	var conn net.Conn
	var jump *Client
	if t.Jump != nil {
		j, err := Dial(ctx, *t.Jump)
		if err != nil {
			return nil, fmt.Errorf("jump host %s: %w", t.Jump.Address, err)
		}
		if conn, err = j.DialContext(ctx, "tcp", addr); err != nil {
			j.Close()
			return nil, fmt.Errorf("via jump host %s: %w", t.Jump.Address, err)
		}
		jump = j
	} else {
		d := net.Dialer{Timeout: dialTimeout}
		var err error
		if conn, err = d.DialContext(ctx, "tcp", addr); err != nil {
			return nil, err
		}
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		if jump != nil {
			jump.Close()
		}
		return nil, err
	}
	return &Client{Client: ssh.NewClient(c, chans, reqs), HostKey: seen, jump: jump}, nil
}

// Fingerprint 返回公钥的 SHA256 指纹，无法解析时返回空。
func Fingerprint(authorizedKey string) string {
	k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorizedKey))
	if err != nil {
		return ""
	}
	return ssh.FingerprintSHA256(k)
}

// Run 以 bash（没有时用 sh）执行脚本，输出写到 out；ctx 取消时关闭会话。返回退出码。
func (c *Client) Run(ctx context.Context, script string, stdin io.Reader, out io.Writer) (int, error) {
	s, err := c.NewSession()
	if err != nil {
		return -1, err
	}
	defer s.Close()
	w := &lockedWriter{w: out} // stdout 与 stderr 由两个 goroutine 并发写入
	s.Stdin, s.Stdout, s.Stderr = stdin, w, w
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Signal(ssh.SIGKILL)
			s.Close()
		case <-done:
		}
	}()
	// 脚本作为单个参数交给 shell，避免远程登录 shell 再解析一遍。
	err = s.Run("if command -v bash >/dev/null 2>&1; then exec bash -c " + Quote(script) + "; else exec sh -c " + Quote(script) + "; fi")
	var exit *ssh.ExitError
	switch {
	case err == nil:
		return 0, nil
	case ctx.Err() != nil:
		return -1, ctx.Err()
	case errors.As(err, &exit):
		return exit.ExitStatus(), fmt.Errorf("exit code %d", exit.ExitStatus())
	default:
		return -1, err
	}
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// Quote 把字符串转义为 POSIX shell 单引号字面量。
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
