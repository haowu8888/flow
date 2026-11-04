// Package connect 通过服务连接发送消息（Bark、企业微信机器人、Telegram、邮件）并测试连接是否可用。
// values 为连接字段的明文（键同 engine.ConnTypes 中的字段 Key）。
package connect

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"strings"
	"time"

	"flow/internal/blob"
)

// Message 是要发送的消息；Body 为纯文本（企业微信、Telegram 以 Markdown 显示）。
type Message struct {
	Title string
	Body  string
	URL   string // 点击跳转的地址（可选）
	// Links 是附加的操作链接（如审批的通过 / 驳回），按顺序显示。
	Links []Link
}

type Link struct {
	Text string
	URL  string
}

var client = &http.Client{Timeout: 15 * time.Second}

// Sendable 判断该类型的连接能否发消息。
func Sendable(typ string) bool {
	switch typ {
	case "bark", "wecom-bot", "telegram", "email":
		return true
	}
	return false
}

// Send 通过连接发送消息；to 仅用于邮件（为空发给 test_to 或发件人）。
func Send(ctx context.Context, typ string, v map[string]string, m Message, to []string) error {
	switch typ {
	case "bark":
		return sendBark(ctx, v, m)
	case "wecom-bot":
		return sendWecom(ctx, v, m)
	case "telegram":
		return sendTelegram(ctx, v, m)
	case "email":
		return sendEmail(v, m, to)
	}
	return fmt.Errorf("connection type %s cannot send messages", typ)
}

// Test 检查连接是否可用：消息类连接发一条测试消息，其他类型做一次只读（或可撤销）的调用。
func Test(ctx context.Context, typ string, v map[string]string) error {
	if Sendable(typ) {
		return Send(ctx, typ, v, Message{Title: "Flow 服务连接测试", Body: "收到这条消息说明服务连接配置正确。"}, nil)
	}
	switch typ {
	case "git":
		return testGit(ctx, v)
	case "npm":
		return testNPM(ctx, v)
	case "oss":
		return testOSS(ctx, v)
	case "cloudreve":
		_, err := CloudreveLogin(ctx, v)
		return err
	}
	return fmt.Errorf("connection type %s cannot be tested", typ)
}

func postJSON(ctx context.Context, u string, body any, header http.Header) ([]byte, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range header {
		req.Header[k] = vs
	}
	return do(req)
}

func do(req *http.Request) ([]byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return data, fmt.Errorf("%s: HTTP %d %s", req.URL.Host, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

func text(m Message) string {
	var b strings.Builder
	b.WriteString(m.Body)
	if m.URL != "" {
		b.WriteString("\n" + m.URL)
	}
	for _, l := range m.Links {
		fmt.Fprintf(&b, "\n%s：%s", l.Text, l.URL)
	}
	return b.String()
}

func sendBark(ctx context.Context, v map[string]string, m Message) error {
	server := strings.TrimRight(v["server"], "/")
	if server == "" {
		server = "https://api.day.app"
	}
	body := map[string]string{"device_key": v["device_key"], "title": m.Title, "body": text(m), "group": "Flow"}
	if m.URL != "" {
		body["url"] = m.URL
	}
	data, err := postJSON(ctx, server+"/push", body, nil)
	if err != nil {
		return err
	}
	var r struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &r) == nil && r.Code != 0 && r.Code != 200 {
		return fmt.Errorf("bark: %s", r.Message)
	}
	return nil
}

func sendWecom(ctx context.Context, v map[string]string, m Message) error {
	content := "**" + m.Title + "**\n" + m.Body
	if m.URL != "" {
		content += "\n[查看详情](" + m.URL + ")"
	}
	for _, l := range m.Links {
		content += "\n[" + l.Text + "](" + l.URL + ")"
	}
	data, err := postJSON(ctx, v["webhook"], map[string]any{"msgtype": "markdown", "markdown": map[string]string{"content": content}}, nil)
	if err != nil {
		return err
	}
	var r struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if json.Unmarshal(data, &r) == nil && r.ErrCode != 0 {
		return fmt.Errorf("wecom: %s", r.ErrMsg)
	}
	return nil
}

func sendTelegram(ctx context.Context, v map[string]string, m Message) error {
	base := strings.TrimRight(v["api_base"], "/")
	if base == "" {
		base = "https://api.telegram.org"
	}
	body := map[string]any{"chat_id": v["chat_id"], "text": "<b>" + escapeHTML(m.Title) + "</b>\n" + escapeHTML(m.Body), "parse_mode": "HTML"}
	var buttons []map[string]string
	if m.URL != "" {
		buttons = append(buttons, map[string]string{"text": "查看详情", "url": m.URL})
	}
	for _, l := range m.Links {
		buttons = append(buttons, map[string]string{"text": l.Text, "url": l.URL})
	}
	if len(buttons) > 0 {
		body["reply_markup"] = map[string]any{"inline_keyboard": [][]map[string]string{buttons}}
	}
	data, err := postJSON(ctx, base+"/bot"+v["bot_token"]+"/sendMessage", body, nil)
	if err != nil {
		// 不在错误里带出令牌
		return errors.New(strings.ReplaceAll(err.Error(), v["bot_token"], "***"))
	}
	var r struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if json.Unmarshal(data, &r) == nil && !r.OK {
		return fmt.Errorf("telegram: %s", r.Description)
	}
	return nil
}

func escapeHTML(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func sendEmail(v map[string]string, m Message, to []string) error {
	from := v["from"]
	if from == "" {
		from = v["username"]
	}
	fromAddr, err := mail.ParseAddress(from)
	if err != nil {
		return fmt.Errorf("invalid sender %q: %w", from, err)
	}
	if len(to) == 0 {
		if t := v["test_to"]; t != "" {
			to = []string{t}
		} else {
			to = []string{fromAddr.Address}
		}
	}
	var rcpt []string
	for _, t := range to {
		a, err := mail.ParseAddress(strings.TrimSpace(t))
		if err != nil {
			return fmt.Errorf("invalid recipient %q", t)
		}
		rcpt = append(rcpt, a.Address)
	}
	var body bytes.Buffer
	fmt.Fprintf(&body, "From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=UTF-8\r\nDate: %s\r\n",
		fromAddr.String(), strings.Join(rcpt, ", "), mimeHeader(m.Title), time.Now().Format(time.RFC1123Z))
	if r := v["reply_to"]; r != "" {
		fmt.Fprintf(&body, "Reply-To: %s\r\n", r)
	}
	body.WriteString("\r\n")
	body.WriteString(emailHTML(m))

	host, port := v["host"], v["port"]
	addr := net.JoinHostPort(host, port)
	var c *smtp.Client
	if v["tls"] == "true" {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 15 * time.Second}, "tcp", addr, &tls.Config{ServerName: host})
		if err != nil {
			return err
		}
		if c, err = smtp.NewClient(conn, host); err != nil {
			return err
		}
	} else {
		conn, err := net.DialTimeout("tcp", addr, 15*time.Second)
		if err != nil {
			return err
		}
		if c, err = smtp.NewClient(conn, host); err != nil {
			return err
		}
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: host}); err != nil {
				return err
			}
		}
	}
	defer c.Close()
	if v["username"] != "" {
		if err := c.Auth(smtp.PlainAuth("", v["username"], v["password"], host)); err != nil {
			return err
		}
	}
	if err := c.Mail(fromAddr.Address); err != nil {
		return err
	}
	for _, r := range rcpt {
		if err := c.Rcpt(r); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(body.Bytes()); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func mimeHeader(s string) string {
	for _, r := range s {
		if r > 127 {
			return "=?UTF-8?B?" + b64(s) + "?="
		}
	}
	return s
}

func emailHTML(m Message) string {
	esc := func(s string) string {
		return strings.ReplaceAll(escapeHTML(s), "\n", "<br>")
	}
	var b strings.Builder
	b.WriteString(`<div style="font-family:-apple-system,'PingFang SC','Microsoft YaHei',sans-serif;max-width:560px;margin:auto;padding:20px;border:1px solid #e7e5e4;border-radius:12px">`)
	fmt.Fprintf(&b, `<h2 style="margin:0 0 12px;font-size:18px;color:#1c1917">%s</h2>`, esc(m.Title))
	fmt.Fprintf(&b, `<p style="color:#44403c;line-height:1.6">%s</p>`, esc(m.Body))
	btn := func(text, u, bg string) {
		fmt.Fprintf(&b, `<a href="%s" style="display:inline-block;margin:8px 8px 0 0;padding:8px 16px;border-radius:8px;background:%s;color:#fff;text-decoration:none">%s</a>`, escapeHTML(u), bg, esc(text))
	}
	if m.URL != "" {
		btn("查看详情", m.URL, "#18181b")
	}
	for i, l := range m.Links {
		bg := "#16a34a"
		if i > 0 {
			bg = "#dc2626"
		}
		btn(l.Text, l.URL, bg)
	}
	b.WriteString(`</div>`)
	return b.String()
}

func testGit(ctx context.Context, v map[string]string) error {
	var req *http.Request
	var err error
	switch v["platform"] {
	case "github":
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+v["token"])
		}
	case "gitea":
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(v["server"], "/")+"/api/v1/user", nil)
		if err == nil {
			req.Header.Set("Authorization", "token "+v["token"])
		}
	case "gitee":
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, "https://gitee.com/api/v5/user?access_token="+url.QueryEscape(v["token"]), nil)
	default:
		return errors.New("通用 Git 连接没有可调用的接口，保存后在流水线里克隆验证")
	}
	if err != nil {
		return err
	}
	_, err = do(req)
	return err
}

func testNPM(ctx context.Context, v map[string]string) error {
	reg := strings.TrimRight(v["registry"], "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reg+"/-/whoami", nil)
	if err != nil {
		return err
	}
	if v["token"] != "" {
		req.Header.Set("Authorization", "Bearer "+v["token"])
	} else {
		req.SetBasicAuth(v["username"], v["password"])
	}
	_, err = do(req)
	return err
}

func testOSS(ctx context.Context, v map[string]string) error {
	if v["bucket"] == "" {
		return errors.New("填写默认 Bucket 后才能测试")
	}
	s, err := blob.NewS3(blob.S3Config{Endpoint: v["endpoint"], Region: v["region"], Bucket: v["bucket"],
		AccessKey: v["access_key_id"], SecretKey: v["access_key_secret"], PathStyle: v["path_style"] == "true"})
	if err != nil {
		return err
	}
	key := ".flow-connection-test"
	if err := s.Put(ctx, key, strings.NewReader("ok"), 2); err != nil {
		return err
	}
	return s.Delete(ctx, key)
}

// CloudreveLogin 用账号密码换取 Cloudreve v4 的访问令牌。
func CloudreveLogin(ctx context.Context, v map[string]string) (string, error) {
	data, err := postJSON(ctx, strings.TrimRight(v["server"], "/")+"/api/v4/session/token",
		map[string]string{"email": v["email"], "password": v["password"]}, nil)
	if err != nil {
		return "", err
	}
	var r struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Token struct {
				AccessToken string `json:"access_token"`
			} `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return "", fmt.Errorf("cloudreve: unexpected response")
	}
	if r.Code != 0 || r.Data.Token.AccessToken == "" {
		return "", fmt.Errorf("cloudreve: %s", r.Msg)
	}
	return r.Data.Token.AccessToken, nil
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
