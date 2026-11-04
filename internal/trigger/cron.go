// Package trigger 实现触发器的纯逻辑：cron 表达式解析，Git Webhook 签名校验、事件解析与分支过滤。
package trigger

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule 是解析后的 5 段 cron 表达式（分 时 日 月 周），在指定时区内求值。
type Schedule struct {
	minute, hour, dom, month, dow uint64
	domStar, dowStar              bool
	loc                           *time.Location
}

type field struct {
	min, max int
	names    map[string]int
}

var (
	minuteField = field{min: 0, max: 59}
	hourField   = field{min: 0, max: 23}
	domField    = field{min: 1, max: 31}
	monthField  = field{min: 1, max: 12, names: map[string]int{
		"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
	}}
	// 周日可写作 0 或 7。
	dowField = field{min: 0, max: 7, names: map[string]int{
		"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
	}}
)

var macros = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

// ParseCron 解析标准 5 段 cron（支持 * , - / 、月份与星期英文缩写、@daily 等宏）。
// timezone 为 IANA 时区名，空表示 Server 本地时区。
func ParseCron(expr, timezone string) (*Schedule, error) {
	loc := time.Local
	if timezone != "" {
		var err error
		if loc, err = time.LoadLocation(timezone); err != nil {
			return nil, fmt.Errorf("invalid timezone %q", timezone)
		}
	}
	expr = strings.TrimSpace(expr)
	if m, ok := macros[strings.ToLower(expr)]; ok {
		expr = m
	}
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return nil, fmt.Errorf("cron expression must have 5 fields (minute hour day month weekday), got %d", len(parts))
	}
	s := &Schedule{loc: loc}
	var err error
	for i, f := range []struct {
		dst  *uint64
		spec field
		name string
	}{
		{&s.minute, minuteField, "minute"},
		{&s.hour, hourField, "hour"},
		{&s.dom, domField, "day of month"},
		{&s.month, monthField, "month"},
		{&s.dow, dowField, "day of week"},
	} {
		if *f.dst, err = parseField(parts[i], f.spec); err != nil {
			return nil, fmt.Errorf("cron %s field %q: %v", f.name, parts[i], err)
		}
	}
	if s.dow&(1<<7) != 0 {
		s.dow |= 1
	}
	s.domStar = parts[2] == "*" || parts[2] == "?"
	s.dowStar = parts[4] == "*" || parts[4] == "?"
	return s, nil
}

func parseField(s string, f field) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(s, ",") {
		rng, stepStr, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("invalid step %q", stepStr)
			}
			step = n
		}
		lo, hi := f.min, f.max
		switch {
		case rng == "*" || rng == "?":
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			var err error
			if lo, err = f.value(a); err != nil {
				return 0, err
			}
			if hi, err = f.value(b); err != nil {
				return 0, err
			}
			if lo > hi {
				return 0, fmt.Errorf("range %q is reversed", rng)
			}
		default:
			v, err := f.value(rng)
			if err != nil {
				return 0, err
			}
			lo = v
			if !hasStep {
				hi = v // "5/10" 表示从 5 开始每 10 个单位
			}
		}
		for v := lo; v <= hi; v += step {
			bits |= 1 << v
		}
	}
	return bits, nil
}

func (f field) value(s string) (int, error) {
	if v, ok := f.names[strings.ToLower(s)]; ok {
		return v, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid value %q", s)
	}
	if v < f.min || v > f.max {
		return 0, fmt.Errorf("value %d out of range %d-%d", v, f.min, f.max)
	}
	return v, nil
}

// Next 返回严格晚于 after 的下一个触发时刻；5 年内无匹配（如 2 月 30 日）返回零值。
// 夏令时：跳过的时刻顺延到下一个存在的时刻，重复的时刻只触发一次。
func (s *Schedule) Next(after time.Time) time.Time {
	after = after.In(s.loc)
	t := after.Truncate(time.Minute).Add(time.Minute)
	limit := t.Year() + 5
	for t.Year() <= limit {
		y, mo, d := t.Date()
		var next time.Time
		switch {
		case s.month&(1<<uint(mo)) == 0:
			next = time.Date(y, mo+1, 1, 0, 0, 0, 0, s.loc)
		case !s.dayMatches(t):
			next = time.Date(y, mo, d+1, 0, 0, 0, 0, s.loc)
		case s.hour&(1<<uint(t.Hour())) == 0:
			next = time.Date(y, mo, d, t.Hour()+1, 0, 0, 0, s.loc)
		case s.minute&(1<<uint(t.Minute())) == 0 || !wall(t).After(wall(after)):
			next = t.Add(time.Minute)
		default:
			return t
		}
		// 目标挂钟时刻落在夏令时跳过区间时，time.Date 可能归一化到更早的时刻，此时逐分钟前进。
		if !next.After(t) {
			next = t.Add(time.Minute)
		}
		t = next
	}
	return time.Time{}
}

// wall 取本地挂钟时间（精确到分钟），用于识别夏令时回拨造成的重复时刻。
func wall(t time.Time) time.Time {
	y, mo, d := t.Date()
	return time.Date(y, mo, d, t.Hour(), t.Minute(), 0, 0, time.UTC)
}

// dayMatches 遵循 Vixie cron：日与周都被限定时满足其一即可，否则两者都须满足。
func (s *Schedule) dayMatches(t time.Time) bool {
	dom := s.dom&(1<<uint(t.Day())) != 0
	dow := s.dow&(1<<uint(t.Weekday())) != 0
	if s.domStar || s.dowStar {
		return dom && dow
	}
	return dom || dow
}
