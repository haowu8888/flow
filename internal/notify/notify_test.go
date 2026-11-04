package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestSend(t *testing.T) {
	type got struct {
		query url.Values
		body  map[string]any
	}
	var last got
	reply := `{"errcode":0}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		last = got{query: r.URL.Query()}
		json.Unmarshal(data, &last.body)
		io.WriteString(w, reply)
	}))
	defer srv.Close()
	s := NewSender()
	s.Now = func() time.Time { return time.UnixMilli(1700000000123) }
	msg := Message{Pipeline: "demo", Number: 7, Status: "failed", Trigger: "Webhook main", Duration: "1m2s",
		URL: "https://flow.example.com/#/runs/abc", Failed: []string{"unit"}}
	ctx := context.Background()

	if err := s.Send(ctx, Target{Type: "dingtalk", URL: srv.URL + "/robot/send?access_token=x", Secret: "SEC"}, msg); err != nil {
		t.Fatal(err)
	}
	if last.query.Get("access_token") != "x" || last.query.Get("timestamp") != "1700000000123" ||
		last.query.Get("sign") != hmacB64("SEC", "1700000000123\nSEC") {
		t.Errorf("dingtalk query = %v", last.query)
	}
	md := last.body["markdown"].(map[string]any)["text"].(string)
	for _, want := range []string{"demo #7 ❌ 失败", "失败任务：unit", "[查看详情](https://flow.example.com/#/runs/abc)"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}

	if err := s.Send(ctx, Target{Type: "feishu", URL: srv.URL, Secret: "SEC"}, msg); err != nil {
		t.Fatal(err)
	}
	if last.body["timestamp"] != "1700000000" || last.body["sign"] != hmacB64("1700000000\nSEC", "") {
		t.Errorf("feishu body = %v", last.body)
	}
	if text := last.body["content"].(map[string]any)["text"].(string); !strings.Contains(text, "详情：https://flow.example.com") {
		t.Errorf("feishu text = %s", text)
	}

	if err := s.Send(ctx, Target{Type: "webhook", URL: srv.URL}, msg); err != nil || last.body["status"] != "failed" {
		t.Errorf("webhook = %v, %v", last.body, err)
	}

	reply = `{"errcode":310000,"errmsg":"sign not match"}`
	if err := s.Send(ctx, Target{Type: "dingtalk", URL: srv.URL}, msg); err == nil || !strings.Contains(err.Error(), "sign not match") {
		t.Errorf("dingtalk error = %v", err)
	}
	reply = `{"code":19021,"msg":"sign match fail"}`
	if err := s.Send(ctx, Target{Type: "feishu", URL: srv.URL}, msg); err == nil {
		t.Error("feishu error not detected")
	}
}
