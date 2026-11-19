package api

import (
	"strings"
	"sync"
	"time"
)

// 登录限流：同一 IP 在窗口期内失败过多，或同一 IP + 用户名连续失败，就暂时拒绝登录。
// 被拒绝的请求不再校验密码，既挡暴力破解，也避免用 PBKDF2 把 CPU 打满。
const (
	loginIPWindow    = 15 * time.Minute
	loginIPMaxFails  = 20
	loginUserMaxFail = 5                // 连续失败达到此次数后开始锁定
	loginUserLockMin = time.Minute      // 首次锁定时长，之后每多失败一次翻倍
	loginUserLockMax = 15 * time.Minute // 锁定时长上限
	loginMaxEntries  = 10000            // 超过后清理已过期的记录
)

type loginEntry struct {
	fails       int
	windowStart time.Time // IP 记录为窗口起点；用户记录为最近一次失败，闲置一个窗口后可被清理
	lockedUntil time.Time
}

type loginLimiter struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]*loginEntry
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{now: time.Now, entries: map[string]*loginEntry{}}
}

func loginIPKey(ip string) string { return "ip:" + ip }

func loginUserKey(ip, username string) string {
	return "user:" + ip + "|" + strings.ToLower(strings.TrimSpace(username))
}

// allow 返回是否允许本次登录尝试；不允许时同时返回需要等待的时长。
func (l *loginLimiter) allow(ip, username string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	var wait time.Duration
	for _, k := range []string{loginIPKey(ip), loginUserKey(ip, username)} {
		if e := l.entries[k]; e != nil && now.Before(e.lockedUntil) {
			wait = max(wait, e.lockedUntil.Sub(now))
		}
	}
	return wait, wait == 0
}

// fail 记录一次密码错误。
func (l *loginLimiter) fail(ip, username string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.prune(now)

	ipe := l.entry(loginIPKey(ip))
	if now.Sub(ipe.windowStart) >= loginIPWindow {
		ipe.fails, ipe.windowStart = 0, now
	}
	if ipe.fails++; ipe.fails >= loginIPMaxFails {
		ipe.lockedUntil = ipe.windowStart.Add(loginIPWindow)
	}

	ue := l.entry(loginUserKey(ip, username))
	if now.Sub(ue.windowStart) >= loginIPWindow {
		ue.fails = 0 // 上次失败已过去一个窗口，重新计数
	}
	ue.windowStart = now
	if ue.fails++; ue.fails >= loginUserMaxFail {
		lock := loginUserLockMax
		if n := ue.fails - loginUserMaxFail; n < 10 {
			lock = min(loginUserLockMin<<n, loginUserLockMax)
		}
		ue.lockedUntil = now.Add(lock)
	}
}

// success 清除该 IP + 用户名的连续失败计数；IP 窗口计数保留。
func (l *loginLimiter) success(ip, username string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, loginUserKey(ip, username))
}

func (l *loginLimiter) entry(k string) *loginEntry {
	e := l.entries[k]
	if e == nil {
		e = &loginEntry{}
		l.entries[k] = e
	}
	return e
}

// prune 在记录过多时删除已解锁且窗口已过期的记录。
func (l *loginLimiter) prune(now time.Time) {
	if len(l.entries) < loginMaxEntries {
		return
	}
	for k, e := range l.entries {
		if !now.Before(e.lockedUntil) && now.Sub(e.windowStart) >= loginIPWindow {
			delete(l.entries, k)
		}
	}
}
