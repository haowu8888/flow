// Package notify 在运行结束时向钉钉 / 飞书 / 企业微信机器人或通用 Webhook 发送通知。
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Message 是一次运行结束的通知内容。
type Message struct {
	Pipeline string   `json:"pipeline"`
	Number   int      `json:"number"`
	Status   string   `json:"status"`
	Trigger  string   `json:"trigger"`
	Duration string   `json:"duration"`
	URL      string   `json:"url,omitempty"` // 运行详情页链接，未配置 FLOW_PUBLIC_URL 时为空
	Failed   []string `json:"failed_jobs,omitempty"`
	// Job 非空时为任务插件的通知；CustomTitle / CustomBody 为插件自定义的标题与正文（已展开变量）。
	Job         string `json:"job,omitempty"`
	CustomTitle string `json:"title,omitempty"`
	CustomBody  string `json:"content,omitempty"`
}

var statusText = map[string]string{"success": "✅ 成功", "failed": "❌ 失败", "canceled": "⚪ 已取消"}

func (m Message) Title() string {
	if m.CustomTitle != "" {
		return m.CustomTitle
	}
	return fmt.Sprintf("流水线 %s #%d %s", m.Pipeline, m.Number, strings.TrimSpace(strings.TrimLeft(statusText[m.Status], "✅❌⚪ ")))
}

func (m Message) Markdown() string {
	if m.CustomBody != "" {
		md := "### " + m.Title() + "\n\n" + m.CustomBody + "\n"
		if m.URL != "" {
			md += "\n[查看详情](" + m.URL + ")\n"
		}
		return md
	}
	var b strings.Builder
	fmt.Fprintf(&b, "### %s #%d %s\n\n", m.Pipeline, m.Number, statusText[m.Status])
	fmt.Fprintf(&b, "- 触发：%s\n- 耗时：%s\n", m.Trigger, m.Duration)
	if len(m.Failed) > 0 {
		fmt.Fprintf(&b, "- 失败任务：%s\n", strings.Join(m.Failed, "、"))
	}
	if m.URL != "" {
		fmt.Fprintf(&b, "\n[查看详情](%s)\n", m.URL)
	}
	return b.String()
}

func (m Message) Text() string {
	text := strings.ReplaceAll(strings.ReplaceAll(m.Markdown(), "### ", ""), "- ", "")
	if m.URL != "" {
		text = strings.Replace(text, "[查看详情]("+m.URL+")", "详情："+m.URL, 1)
	}
	return text
}

// Target 是解析好密钥后的渠道配置。
type Target struct {
	Type   string
	URL    string
	Secret string
}

type Sender struct {
	HTTP *http.Client
	Now  func() time.Time
}

func NewSender() *Sender {
	return &Sender{HTTP: &http.Client{Timeout: 10 * time.Second}, Now: time.Now}
}

func (s *Sender) Send(ctx context.Context, t Target, m Message) error {
	endpoint := t.URL
	var body any
	switch t.Type {
	case "dingtalk":
		if t.Secret != "" {
			ts := strconv.FormatInt(s.Now().UnixMilli(), 10)
			sign := url.QueryEscape(hmacB64(t.Secret, ts+"\n"+t.Secret))
			endpoint = appendQuery(endpoint, "timestamp="+ts+"&sign="+sign)
		}
		body = map[string]any{"msgtype": "markdown", "markdown": map[string]string{"title": m.Title(), "text": m.Markdown()}}
	case "feishu":
		payload := map[string]any{"msg_type": "text", "content": map[string]string{"text": m.Text()}}
		if t.Secret != "" {
			ts := strconv.FormatInt(s.Now().Unix(), 10)
			payload["timestamp"] = ts
			payload["sign"] = hmacB64(ts+"\n"+t.Secret, "") // 飞书：以 timestamp\nsecret 为密钥对空串签名
		}
		body = payload
	case "wecom":
		body = map[string]any{"msgtype": "markdown", "markdown": map[string]string{"content": m.Markdown()}}
	case "webhook":
		body = m
	default:
		return fmt.Errorf("unknown notify type %q", t.Type)
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %s: %s", t.Type, resp.Status, bytes.TrimSpace(respBody))
	}
	// 钉钉 / 企业微信 / 飞书在 HTTP 200 中用 errcode / code 表示失败。
	var r struct {
		ErrCode *int   `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
		Code    *int   `json:"code"`
		Msg     string `json:"msg"`
	}
	if json.Unmarshal(respBody, &r) == nil {
		if r.ErrCode != nil && *r.ErrCode != 0 {
			return fmt.Errorf("%s: errcode %d: %s", t.Type, *r.ErrCode, r.ErrMsg)
		}
		if t.Type == "feishu" && r.Code != nil && *r.Code != 0 {
			return fmt.Errorf("feishu: code %d: %s", *r.Code, r.Msg)
		}
	}
	return nil
}

func hmacB64(key, msg string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(msg))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func appendQuery(u, q string) string {
	if strings.Contains(u, "?") {
		return u + "&" + q
	}
	return u + "?" + q
}
