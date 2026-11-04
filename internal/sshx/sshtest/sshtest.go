// Package sshtest 提供测试用的 SSH 服务端：认证后用本机 sh 执行 exec 请求，用于验证主机部署。
package sshtest

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"golang.org/x/crypto/ssh"
)

// Server 是运行中的测试服务端。
type Server struct {
	Host, User, Password string
	Port                 int
	HostKey              string // authorized_keys 格式
	ClientKey            string // 被授权的客户端私钥（PEM）

	mu       sync.Mutex
	commands []string
	forwards []string
}

// Commands 返回收到的 exec 命令。
func (s *Server) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...)
}

// Start 启动服务端，测试结束时关闭。
func Start(t testing.TB) *Server {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, _ := ssh.NewSignerFromKey(hostPriv)
	clientPub, clientPriv, _ := ed25519.GenerateKey(rand.Reader)
	sshClientPub, _ := ssh.NewPublicKey(clientPub)
	block, _ := ssh.MarshalPrivateKey(clientPriv, "")

	s := &Server{User: "deploy", Password: "pa55", HostKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostSigner.PublicKey()))),
		ClientKey: string(pem.EncodeToMemory(block))}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if c.User() == s.User && string(pass) == s.Password {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if c.User() == s.User && string(key.Marshal()) == string(sshClientPub.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
	}
	cfg.AddHostKey(hostSigner)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	s.Host = host
	s.Port, _ = strconv.Atoi(port)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c, cfg)
		}
	}()
	return s
}

func (s *Server) serve(c net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		c.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() == "direct-tcpip" {
			go s.forward(nc) // 作为跳板机转发到目标
			continue
		}
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		ch, in, err := nc.Accept()
		if err != nil {
			continue
		}
		go s.session(ch, in)
	}
}

// Forwards 返回经本机转发过的目标地址。
func (s *Server) Forwards() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.forwards...)
}

func (s *Server) forward(nc ssh.NewChannel) {
	var req struct {
		Host       string
		Port       uint32
		OriginHost string
		OriginPort uint32
	}
	if err := ssh.Unmarshal(nc.ExtraData(), &req); err != nil {
		nc.Reject(ssh.ConnectionFailed, "bad request")
		return
	}
	addr := net.JoinHostPort(req.Host, strconv.Itoa(int(req.Port)))
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		nc.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	s.mu.Lock()
	s.forwards = append(s.forwards, addr)
	s.mu.Unlock()
	ch, reqs, err := nc.Accept()
	if err != nil {
		conn.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	go func() {
		io.Copy(ch, conn)
		ch.CloseWrite()
	}()
	io.Copy(conn, ch)
	conn.Close()
	ch.Close()
}

func (s *Server) session(ch ssh.Channel, in <-chan *ssh.Request) {
	defer ch.Close()
	var cmd *exec.Cmd
	for req := range in {
		switch req.Type {
		case "exec":
			if len(req.Payload) < 4 {
				req.Reply(false, nil)
				continue
			}
			n := binary.BigEndian.Uint32(req.Payload)
			command := string(req.Payload[4 : 4+n])
			s.mu.Lock()
			s.commands = append(s.commands, command)
			s.mu.Unlock()
			req.Reply(true, nil)
			cmd = exec.Command("sh", "-c", command)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Stdout, cmd.Stderr = ch, ch.Stderr()
			stdin, _ := cmd.StdinPipe()
			if err := cmd.Start(); err != nil {
				sendExit(ch, 127)
				return
			}
			go func() {
				io.Copy(stdin, ch)
				stdin.Close()
			}()
			go func() {
				code := 0
				if err := cmd.Wait(); err != nil {
					code = 1
					var ee *exec.ExitError
					if errors.As(err, &ee) && ee.ExitCode() >= 0 {
						code = ee.ExitCode()
					}
				}
				sendExit(ch, code)
				ch.Close()
			}()
		case "signal":
			if cmd != nil && cmd.Process != nil {
				syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		default:
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}
}

func sendExit(ch ssh.Channel, code int) {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(code))
	ch.SendRequest("exit-status", false, b)
}
