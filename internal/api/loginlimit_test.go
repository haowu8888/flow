package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"flow/internal/store"
)

func TestLoginLimiter(t *testing.T) {
	now := time.Unix(0, 0)
	l := newLoginLimiter()
	l.now = func() time.Time { return now }

	for i := 0; i < loginUserMaxFail; i++ {
		if _, ok := l.allow("1.1.1.1", "admin"); !ok {
			t.Fatalf("attempt %d blocked", i+1)
		}
		l.fail("1.1.1.1", "admin")
	}
	if wait, ok := l.allow("1.1.1.1", " Admin "); ok || wait != loginUserLockMin {
		t.Fatalf("after %d fails: wait=%v ok=%v", loginUserMaxFail, wait, ok)
	}
	// 其他用户名、其他 IP 不受影响
	if _, ok := l.allow("1.1.1.1", "bob"); !ok {
		t.Fatal("other user blocked")
	}
	if _, ok := l.allow("2.2.2.2", "admin"); !ok {
		t.Fatal("other ip blocked")
	}

	// 解锁后再错一次，锁定时长翻倍
	now = now.Add(loginUserLockMin)
	if _, ok := l.allow("1.1.1.1", "admin"); !ok {
		t.Fatal("still blocked after lock expired")
	}
	l.fail("1.1.1.1", "admin")
	if wait, _ := l.allow("1.1.1.1", "admin"); wait != 2*loginUserLockMin {
		t.Fatalf("second lock = %v", wait)
	}

	// 登录成功清零
	now = now.Add(2 * loginUserLockMin)
	l.success("1.1.1.1", "admin")
	l.fail("1.1.1.1", "admin")
	if _, ok := l.allow("1.1.1.1", "admin"); !ok {
		t.Fatal("success did not reset")
	}

	// 同一 IP 换用户名也逃不过 IP 窗口上限
	now = now.Add(loginIPWindow) // 新窗口
	for i := 0; i < loginIPMaxFails; i++ {
		l.fail("3.3.3.3", fmt.Sprintf("user%d", i))
	}
	if wait, ok := l.allow("3.3.3.3", "fresh"); ok || wait != loginIPWindow {
		t.Fatalf("ip limit: wait=%v ok=%v", wait, ok)
	}
	now = now.Add(loginIPWindow)
	if _, ok := l.allow("3.3.3.3", "fresh"); !ok {
		t.Fatal("ip still blocked after window")
	}
}

func TestLoginRateLimitHTTP(t *testing.T) {
	st := newStack(t, store.NewMemory())
	anon := st.as("")
	login := func(password string) (int, string) {
		return anon.do(t, http.MethodPost, "/api/auth/login", fmt.Sprintf(`{"username":"admin","password":%q}`, password))
	}
	for i := 0; i < loginUserMaxFail; i++ {
		if code, body := login("wrong"); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d: %s", i+1, code, body)
		}
	}
	// 锁定期间即使密码正确也拒绝
	if code, body := login(adminPassword); code != http.StatusTooManyRequests {
		t.Fatalf("locked login = %d: %s", code, body)
	}
}

func TestClientIP(t *testing.T) {
	cases := []struct {
		remote, xff, real, want string
	}{
		{"203.0.113.5:1234", "1.2.3.4", "", "203.0.113.5"},              // 公网直连忽略转发头
		{"127.0.0.1:1234", "", "", "127.0.0.1"},                         // 无转发头
		{"172.18.0.1:1234", "6.6.6.6, 203.0.113.9", "", "203.0.113.9"},  // 取最右边的非内网地址
		{"172.18.0.1:1234", "203.0.113.9, 10.0.0.2", "", "203.0.113.9"}, // 跳过链路上的内网代理
		{"127.0.0.1:1234", "", "203.0.113.7", "203.0.113.7"},            // 只有 X-Real-IP
		{"127.0.0.1:1234", "garbage", "203.0.113.7", "203.0.113.7"},     // 非法 XFF 回落到 X-Real-IP
		{"127.0.0.1:1234", "10.0.0.3", "", "10.0.0.3"},                  // 全是内网时取最左边
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if c.real != "" {
			r.Header.Set("X-Real-IP", c.real)
		}
		if got := clientIP(r); got != c.want {
			t.Errorf("clientIP(%s, xff=%q, real=%q) = %s, want %s", c.remote, c.xff, c.real, got, c.want)
		}
	}
}
