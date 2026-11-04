package sshx_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"flow/internal/sshx"
	"flow/internal/sshx/sshtest"
)

func TestDialRun(t *testing.T) {
	srv := sshtest.Start(t)
	ctx := context.Background()
	for _, tgt := range []sshx.Target{
		{Address: srv.Host, Port: srv.Port, User: srv.User, Password: srv.Password},
		{Address: srv.Host, Port: srv.Port, User: srv.User, PrivateKey: srv.ClientKey, HostKey: srv.HostKey},
	} {
		c, err := sshx.Dial(ctx, tgt)
		if err != nil {
			t.Fatal(err)
		}
		if c.HostKey != srv.HostKey {
			t.Errorf("host key = %q", c.HostKey)
		}
		var out bytes.Buffer
		code, err := c.Run(ctx, "echo \"it's $((1+2))\"; cat; exit 4", strings.NewReader("from stdin"), &out)
		if code != 4 || err == nil || out.String() != "it's 3\nfrom stdin" {
			t.Errorf("run = %d %v %q", code, err, out.String())
		}
		c.Close()
	}

	// 主机公钥不一致、密码错误
	other := sshtest.Start(t)
	if _, err := sshx.Dial(ctx, sshx.Target{Address: srv.Host, Port: srv.Port, User: srv.User, Password: srv.Password, HostKey: other.HostKey}); !errors.Is(err, sshx.ErrHostKeyMismatch) {
		t.Errorf("mismatch err = %v", err)
	}
	if _, err := sshx.Dial(ctx, sshx.Target{Address: srv.Host, Port: srv.Port, User: srv.User, Password: "wrong"}); err == nil {
		t.Error("wrong password accepted")
	}
	if sshx.Fingerprint(srv.HostKey) == "" || !strings.HasPrefix(sshx.Fingerprint(srv.HostKey), "SHA256:") {
		t.Errorf("fingerprint = %q", sshx.Fingerprint(srv.HostKey))
	}

	// 取消时结束远程命令
	c, _ := sshx.Dial(ctx, sshx.Target{Address: srv.Host, Port: srv.Port, User: srv.User, Password: srv.Password})
	defer c.Close()
	cctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Run(cctx, "sleep 30", nil, &bytes.Buffer{}); err == nil || time.Since(start) > 5*time.Second {
		t.Errorf("cancel: err %v after %s", err, time.Since(start))
	}
}

func TestJumpHost(t *testing.T) {
	jump, target := sshtest.Start(t), sshtest.Start(t)
	ctx := context.Background()
	c, err := sshx.Dial(ctx, sshx.Target{Address: target.Host, Port: target.Port, User: target.User, Password: target.Password, HostKey: target.HostKey,
		Jump: &sshx.Target{Address: jump.Host, Port: jump.Port, User: jump.User, PrivateKey: jump.ClientKey, HostKey: jump.HostKey}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var out bytes.Buffer
	if _, err := c.Run(ctx, "echo via jump", nil, &out); err != nil || out.String() != "via jump\n" {
		t.Fatalf("run = %q %v", out.String(), err)
	}
	if f := jump.Forwards(); len(f) != 1 || len(jump.Commands()) != 0 || len(target.Commands()) != 1 {
		t.Errorf("forwards %v jump cmds %v target cmds %v", f, jump.Commands(), target.Commands())
	}
	// 跳板机认证失败时报告是跳板机的问题。
	_, err = sshx.Dial(ctx, sshx.Target{Address: target.Host, Port: target.Port, User: target.User, Password: target.Password,
		Jump: &sshx.Target{Address: jump.Host, Port: jump.Port, User: jump.User, Password: "wrong"}})
	if err == nil || !strings.Contains(err.Error(), "jump host") {
		t.Errorf("bad jump err = %v", err)
	}
}
