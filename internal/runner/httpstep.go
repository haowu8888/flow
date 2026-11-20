package runner

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"flow/internal/protocol"
)

const (
	httpLogBytes    = 2000
	httpOutputBytes = 4000
	httpMaxBody     = 16 << 20
)

// httpRequest 是 http-request 步骤：由 Runner 直接发请求，不依赖构建环境里的 curl / python3。
// 输入与输出和脚本版一致（部署任务在目标主机上仍执行脚本版）：请求失败或状态码不符时失败，
// 输出 HTTP_STATUS、HTTP_BODY（前 4000 字节），填了 json_path 时输出 HTTP_VALUE。
func httpRequest(ctx context.Context, r *Runner, a *protocol.ClaimResponse, sc *stepContext, env map[string]string, out io.Writer) (int, error) {
	method := strings.ToUpper(strings.TrimSpace(env["INPUT_METHOD"]))
	if method == "" {
		method = http.MethodGet
	}
	target := strings.TrimSpace(env["INPUT_URL"])
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return 1, fmt.Errorf("请求地址无效：%s", target)
	}
	timeout, err := intInput(env, "INPUT_TIMEOUT", 30)
	if err != nil {
		return 1, err
	}
	retries, err := intInput(env, "INPUT_RETRIES", 0)
	if err != nil {
		return 1, err
	}
	header := http.Header{}
	host := ""
	for _, line := range strings.Split(env["INPUT_HEADERS"], "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(k) == "" {
			return 1, fmt.Errorf("请求头格式应为 Name: Value：%s", line)
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if strings.EqualFold(k, "Host") {
			host = v
			continue
		}
		header.Add(k, v)
	}
	body := env["INPUT_BODY"]
	if body != "" && header.Get("Content-Type") == "" {
		header.Set("Content-Type", env["INPUT_CONTENT_TYPE"])
	}

	client := &http.Client{Transport: &http.Transport{
		Proxy:           proxyFunc(env),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: truthy(env["INPUT_INSECURE"])},
	}}
	var (
		code int
		data []byte
	)
	for attempt := 0; ; attempt++ {
		code, data, err = doHTTP(ctx, client, method, target, host, header, body, time.Duration(timeout)*time.Second)
		if attempt >= retries || !retryable(code, err) || ctx.Err() != nil {
			break
		}
		wait := time.Second << min(attempt, 6)
		fmt.Fprintf(out, "[WARN] 第 %d 次请求失败（%s），%s 后重试\n", attempt+1, httpFailure(code, err), wait)
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
	}
	if err != nil {
		return 1, fmt.Errorf("请求失败：%w", err)
	}
	fmt.Fprintf(out, "[INFO] HTTP %d\n", code)
	fmt.Fprintf(out, "%s\n", truncUTF8(data, httpLogBytes))
	status := strconv.Itoa(code)
	sc.setOutput(out, "HTTP_STATUS", status)
	sc.setOutput(out, "HTTP_BODY", strings.NewReplacer("\r", " ", "\n", " ").Replace(string(truncUTF8(data, httpOutputBytes))))
	if p := strings.TrimSpace(env["INPUT_JSON_PATH"]); p != "" {
		v, err := jsonPath(data, p)
		if err != nil {
			return 1, err
		}
		sc.setOutput(out, "HTTP_VALUE", v)
		fmt.Fprintf(out, "[INFO] HTTP_VALUE=%s\n", v)
	}
	expect := env["INPUT_EXPECT"]
	if strings.TrimSpace(expect) == "" {
		expect = "2xx"
	}
	if !statusMatches(status, expect) {
		return 1, fmt.Errorf("状态码 %d 不是期望的 %s", code, expect)
	}
	fmt.Fprintln(out, "[SUCCESS] 请求成功")
	return 0, nil
}

func doHTTP(ctx context.Context, client *http.Client, method, target, host string, header http.Header, body string, timeout time.Duration) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header = header.Clone()
	if host != "" {
		req.Host = host
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, httpMaxBody))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, data, nil
}

// retryable 与 curl --retry 一致：网络错误、超时与 408 / 429 / 5xx（501 除外）重试。
func retryable(code int, err error) bool {
	if err != nil {
		return true
	}
	return code == http.StatusRequestTimeout || code == http.StatusTooManyRequests ||
		code == http.StatusInternalServerError || code == http.StatusBadGateway ||
		code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout
}

func httpFailure(code int, err error) string {
	if err != nil {
		return err.Error()
	}
	return "HTTP " + strconv.Itoa(code)
}

// statusMatches 判断状态码是否符合期望：逗号分隔的 200、2xx 等。
func statusMatches(code, expect string) bool {
	for _, e := range strings.Split(expect, ",") {
		e = strings.ToLower(strings.TrimSpace(e))
		if len(e) == 3 && strings.HasSuffix(e, "xx") {
			if code[:1] == e[:1] {
				return true
			}
		} else if e == code {
			return true
		}
	}
	return false
}

// jsonPath 按点分路径（数组用下标，如 data.items.0.id）取值：字符串原样输出，其余输出为 JSON。
func jsonPath(data []byte, path string) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", fmt.Errorf("响应不是 JSON，无法取 %s", path)
	}
	for _, k := range strings.Split(path, ".") {
		switch x := v.(type) {
		case map[string]any:
			val, ok := x[k]
			if !ok {
				return "", fmt.Errorf("响应里没有 %s", path)
			}
			v = val
		case []any:
			i, err := strconv.Atoi(k)
			if err != nil || i < 0 || i >= len(x) {
				return "", fmt.Errorf("响应里没有 %s", path)
			}
			v = x[i]
		default:
			return "", fmt.Errorf("响应里没有 %s", path)
		}
	}
	if s, ok := v.(string); ok {
		return s, nil
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// proxyFunc 让步骤变量里的 HTTP(S)_PROXY / NO_PROXY 生效（与脚本里的 curl 一致），没有时用 Runner 进程的设置。
func proxyFunc(env map[string]string) func(*http.Request) (*url.URL, error) {
	get := func(k string) string {
		if v := env[strings.ToLower(k)]; v != "" {
			return v
		}
		return env[k]
	}
	if get("HTTP_PROXY") == "" && get("HTTPS_PROXY") == "" {
		return http.ProxyFromEnvironment
	}
	return func(req *http.Request) (*url.URL, error) {
		p := get("HTTP_PROXY")
		if req.URL.Scheme == "https" {
			p = get("HTTPS_PROXY")
		}
		if p == "" || noProxy(req.URL.Hostname(), get("NO_PROXY")) {
			return nil, nil
		}
		if !strings.Contains(p, "://") {
			p = "http://" + p
		}
		return url.Parse(p)
	}
}

func noProxy(host, list string) bool {
	for _, e := range strings.Split(list, ",") {
		e = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(e)), ".")
		if e == "*" {
			return true
		}
		if e == "" {
			continue
		}
		if h, _, err := net.SplitHostPort(e); err == nil {
			e = h
		}
		if host == e || strings.HasSuffix(host, "."+e) {
			return true
		}
	}
	return false
}

func intInput(env map[string]string, key string, def int) (int, error) {
	s := strings.TrimSpace(env[key])
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, errors.New(strings.ToLower(strings.TrimPrefix(key, "INPUT_")) + " 应为非负整数：" + s)
	}
	return n, nil
}

// truthy 与脚本里的 truthy 一致：空、false、0、no、n、off、否 为假。
func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "false", "0", "no", "n", "off", "否":
		return false
	}
	return true
}

// truncUTF8 截取前 n 字节，不切断多字节字符。
func truncUTF8(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	cut := n
	for cut > n-utf8.UTFMax && cut > 0 && !utf8.RuneStart(b[cut]) {
		cut--
	}
	return b[:cut]
}
