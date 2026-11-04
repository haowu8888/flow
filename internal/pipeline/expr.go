package pipeline

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// 任务的运行条件（if）：开始执行前求值，为假则跳过。
//
//	FLOW_BRANCH == 'main' && (TARGET == 'prod' || FORCE)
//	FLOW_BRANCH =~ '^release/' && !SKIP_DEPLOY
//
// 支持 == != =~（正则匹配）!~ && || ! 与括号；变量直接写名字（也可写 ${NAME}），未定义为空字符串；
// 字符串用单引号或双引号；单独的变量或字面量按「真值」判断：非空且不是 false / 0 / no / off / 否。

// Cond 是解析后的运行条件。
type Cond struct {
	src  string
	root node
}

type node interface {
	eval(vars map[string]string) (string, error)
}

// ParseCond 解析运行条件；空字符串返回 nil（总是运行）。
func ParseCond(src string) (*Cond, error) {
	if strings.TrimSpace(src) == "" {
		return nil, nil
	}
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	n, err := p.or()
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.toks) {
		return nil, fmt.Errorf("unexpected %q", p.toks[p.pos].v)
	}
	return &Cond{src: src, root: n}, nil
}

// Eval 按变量求值。
func (c *Cond) Eval(vars map[string]string) (bool, error) {
	if c == nil {
		return true, nil
	}
	v, err := c.root.eval(vars)
	if err != nil {
		return false, err
	}
	return truthy(v), nil
}

func (c *Cond) String() string { return c.src }

func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "false", "0", "no", "off", "否":
		return false
	}
	return true
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

type tok struct {
	kind byte // s 字符串 i 标识符 o 运算符 ( )
	v    string
}

func lex(src string) ([]tok, error) {
	var out []tok
	rs := []rune(src)
	for i := 0; i < len(rs); {
		r := rs[i]
		switch {
		case unicode.IsSpace(r):
			i++
		case r == '(' || r == ')':
			out = append(out, tok{kind: byte(r), v: string(r)})
			i++
		case r == '\'' || r == '"':
			j := i + 1
			var b strings.Builder
			for ; j < len(rs) && rs[j] != r; j++ {
				if rs[j] == '\\' && j+1 < len(rs) {
					j++
				}
				b.WriteRune(rs[j])
			}
			if j >= len(rs) {
				return nil, errors.New("unterminated string")
			}
			out = append(out, tok{kind: 's', v: b.String()})
			i = j + 1
		case strings.ContainsRune("=!&|~", r):
			two := ""
			if i+1 < len(rs) {
				two = string(rs[i : i+2])
			}
			switch two {
			case "==", "!=", "=~", "!~", "&&", "||":
				out = append(out, tok{kind: 'o', v: two})
				i += 2
			default:
				if r == '!' {
					out = append(out, tok{kind: 'o', v: "!"})
					i++
				} else {
					return nil, fmt.Errorf("unexpected %q", string(r))
				}
			}
		case r == '$' && i+1 < len(rs) && rs[i+1] == '{':
			j := i + 2
			for j < len(rs) && rs[j] != '}' {
				j++
			}
			if j >= len(rs) {
				return nil, errors.New("unterminated ${")
			}
			out = append(out, tok{kind: 'i', v: string(rs[i+2 : j])})
			i = j + 1
		default:
			j := i
			for j < len(rs) && (unicode.IsLetter(rs[j]) || unicode.IsDigit(rs[j]) || strings.ContainsRune("_.-/:", rs[j])) {
				j++
			}
			if j == i {
				return nil, fmt.Errorf("unexpected %q", string(r))
			}
			w := string(rs[i:j])
			if ValidEnvName(w) {
				out = append(out, tok{kind: 'i', v: w})
			} else {
				out = append(out, tok{kind: 's', v: w}) // 数字等裸字面量
			}
			i = j
		}
	}
	return out, nil
}

type parser struct {
	toks []tok
	pos  int
}

func (p *parser) peek(v string) bool {
	return p.pos < len(p.toks) && p.toks[p.pos].kind == 'o' && p.toks[p.pos].v == v
}

func (p *parser) or() (node, error) {
	l, err := p.and()
	for err == nil && p.peek("||") {
		p.pos++
		var r node
		if r, err = p.and(); err == nil {
			l = logic{op: "||", l: l, r: r}
		}
	}
	return l, err
}

func (p *parser) and() (node, error) {
	l, err := p.unary()
	for err == nil && p.peek("&&") {
		p.pos++
		var r node
		if r, err = p.unary(); err == nil {
			l = logic{op: "&&", l: l, r: r}
		}
	}
	return l, err
}

func (p *parser) unary() (node, error) {
	if p.peek("!") {
		p.pos++
		n, err := p.unary()
		return not{n}, err
	}
	return p.cmp()
}

func (p *parser) cmp() (node, error) {
	l, err := p.operand()
	if err != nil {
		return nil, err
	}
	for _, op := range []string{"==", "!=", "=~", "!~"} {
		if p.peek(op) {
			p.pos++
			r, err := p.operand()
			if err != nil {
				return nil, err
			}
			c := compare{op: op, l: l, r: r}
			if op == "=~" || op == "!~" {
				lit, ok := r.(literal)
				if !ok {
					return nil, fmt.Errorf("right side of %s must be a quoted regexp", op)
				}
				re, err := regexp.Compile(string(lit))
				if err != nil {
					return nil, fmt.Errorf("invalid regexp %q: %v", string(lit), err)
				}
				c.re = re
			}
			return c, nil
		}
	}
	return l, nil
}

func (p *parser) operand() (node, error) {
	if p.pos >= len(p.toks) {
		return nil, errors.New("unexpected end of condition")
	}
	t := p.toks[p.pos]
	p.pos++
	switch t.kind {
	case 's':
		return literal(t.v), nil
	case 'i':
		return ident(t.v), nil
	case '(':
		n, err := p.or()
		if err != nil {
			return nil, err
		}
		if p.pos >= len(p.toks) || p.toks[p.pos].kind != ')' {
			return nil, errors.New("missing )")
		}
		p.pos++
		return n, nil
	}
	return nil, fmt.Errorf("unexpected %q", t.v)
}

type literal string
type ident string
type not struct{ n node }
type logic struct {
	op   string
	l, r node
}
type compare struct {
	op   string
	l, r node
	re   *regexp.Regexp
}

func (l literal) eval(map[string]string) (string, error) { return string(l), nil }
func (i ident) eval(v map[string]string) (string, error) { return v[string(i)], nil }

func (n not) eval(v map[string]string) (string, error) {
	s, err := n.n.eval(v)
	return boolStr(!truthy(s)), err
}

func (n logic) eval(v map[string]string) (string, error) {
	l, err := n.l.eval(v)
	if err != nil {
		return "", err
	}
	if n.op == "&&" && !truthy(l) || n.op == "||" && truthy(l) {
		return boolStr(truthy(l)), nil
	}
	r, err := n.r.eval(v)
	return boolStr(truthy(r)), err
}

func (n compare) eval(v map[string]string) (string, error) {
	l, err := n.l.eval(v)
	if err != nil {
		return "", err
	}
	if n.re != nil {
		return boolStr(n.re.MatchString(l) == (n.op == "=~")), nil
	}
	r, err := n.r.eval(v)
	return boolStr((l == r) == (n.op == "==")), err
}
