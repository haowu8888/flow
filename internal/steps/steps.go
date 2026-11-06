// Package steps 是内置步骤库：步骤写 kind: <类型> 与 with: <输入>，创建运行时展开为脚本，
// 输入以 INPUT_<名称大写> 注入（值中的 ${变量} 由 Runner 按任务变量展开），引用的服务连接以 CONN_<字段> 注入。
// artifact-upload / artifact-download 由 Runner 直接执行（Native）。
package steps

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
)

// Input 是步骤输入。
type Input struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Default     string   `json:"default,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Description string   `json:"description,omitempty"`
	Widget      string   `json:"widget,omitempty"` // text（默认）/ textarea / select / checkbox / code
	Options     []Option `json:"options,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Advanced    bool     `json:"advanced,omitempty"`
}

type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Kind 是一种内置步骤。
type Kind struct {
	Kind        string `json:"kind"`
	Category    string `json:"category"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Connection  string `json:"connection,omitempty"` // 需要的服务连接类型
	// ConnectionOptional 为 true 时服务连接选填（如构建物上传：不选存在 Server）。
	ConnectionOptional bool     `json:"connection_optional,omitempty"`
	Image              string   `json:"image,omitempty"` // 建议的构建镜像（展示用）
	Inputs             []Input  `json:"inputs"`
	Outputs            []string `json:"outputs,omitempty"` // 写入的任务输出
	Native             bool     `json:"native,omitempty"`
	script             string
}

// Categories 是步骤分类（展示顺序）。
var Categories = []Option{
	{"build", "构建"}, {"docker", "Docker"}, {"code", "代码"}, {"test", "测试"}, {"tool", "工具"},
	{"download", "下载"}, {"upload", "上传"}, {"drive", "网盘"}, {"notify", "通知"}, {"publish", "发布"},
}

var (
	catalog     map[string]*Kind
	catalogOnce sync.Once
)

func load() {
	catalogOnce.Do(func() {
		catalog = map[string]*Kind{}
		for _, k := range defs {
			if catalog[k.Kind] != nil {
				panic("duplicate step kind " + k.Kind)
			}
			catalog[k.Kind] = k
		}
	})
}

// All 按分类与定义顺序返回全部步骤类型。
func All() []*Kind {
	load()
	order := map[string]int{}
	for i, c := range Categories {
		order[c.Value] = i
	}
	out := make([]*Kind, 0, len(defs))
	out = append(out, defs...)
	sort.SliceStable(out, func(i, j int) bool { return order[out[i].Category] < order[out[j].Category] })
	return out
}

// Get 返回步骤类型。
func Get(kind string) *Kind {
	load()
	return catalog[kind]
}

// EnvName 是输入在脚本中的环境变量名。
func EnvName(key string) string { return "INPUT_" + strings.ToUpper(key) }

// Expand 校验输入并返回步骤的脚本与环境变量（INPUT_*，未填的取默认值）；Native 步骤返回空脚本。
func (k *Kind) Expand(with map[string]string) (script string, env map[string]string, err error) {
	env = map[string]string{}
	var missing []string
	for name := range with {
		if !slices.ContainsFunc(k.Inputs, func(in Input) bool { return in.Key == name }) {
			return "", nil, fmt.Errorf("unknown input %q for %s", name, k.Kind)
		}
	}
	for _, in := range k.Inputs {
		v, ok := with[in.Key]
		if !ok || v == "" {
			v = in.Default
		}
		if in.Required && strings.TrimSpace(v) == "" {
			missing = append(missing, in.Label)
		}
		if in.Widget == "select" && v != "" && !strings.Contains(v, "${") &&
			!slices.ContainsFunc(in.Options, func(o Option) bool { return o.Value == v }) {
			return "", nil, fmt.Errorf("input %s of %s must be one of %s", in.Key, k.Kind, optionValues(in.Options))
		}
		env[EnvName(in.Key)] = v
	}
	if len(missing) > 0 {
		return "", nil, fmt.Errorf("%s: missing required input(s) %s", k.Label, strings.Join(missing, "、"))
	}
	if k.Native {
		return "", env, nil
	}
	return prelude + k.script, env, nil
}

func optionValues(opts []Option) string {
	vs := make([]string, len(opts))
	for i, o := range opts {
		vs[i] = o.Value
	}
	return strings.Join(vs, ", ")
}

func opts(pairs ...string) []Option {
	var out []Option
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, Option{pairs[i], pairs[i+1]})
	}
	return out
}

// prelude 是所有脚本共用的函数：日志、下载缓存、导出变量 / PATH / 输出。
const prelude = `set -eu
info() { echo "[INFO] $*"; }
ok()   { echo "[SUCCESS] $*"; }
warn() { echo "[WARN] $*"; }
die()  { echo "[ERROR] $*" >&2; exit 1; }
truthy() { case "$(printf '%s' "${1:-}" | tr 'A-Z' 'a-z')" in ''|false|0|no|n|off|否) return 1 ;; esac; return 0; }
# export_env KEY VALUE：后续步骤可见
export_env() { export "$1=$2"; [ -n "${FLOW_ENV:-}" ] && printf '%s=%s\n' "$1" "$2" >> "$FLOW_ENV"; return 0; }
# add_path DIR：后续步骤 PATH 前加上该目录
add_path() { export PATH="$1:$PATH"; [ -n "${FLOW_PATH:-}" ] && printf '%s\n' "$1" >> "$FLOW_PATH"; return 0; }
# set_output KEY VALUE：任务输出（后续步骤与下游任务可见）
set_output() { [ -n "${FLOW_OUTPUT:-}" ] && printf '%s=%s\n' "$1" "$2" >> "$FLOW_OUTPUT"; export_env "$1" "$2"; }
FLOW_TOOLS="${FLOW_TOOLS:-$HOME/.flow-tools}"
DL_DIR="$FLOW_TOOLS/downloads"
mkdir -p "$DL_DIR"
case "$(uname -m)" in
  x86_64|amd64) ARCH=x64; GOARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64; GOARCH=arm64 ;;
  *) ARCH="$(uname -m)"; GOARCH="$ARCH" ;;
esac
case "$(uname -s)" in Darwin) OS=darwin ;; *) OS=linux ;; esac
need() { command -v "$1" >/dev/null 2>&1 || die "构建环境里没有 $1，请换一个带 $1 的镜像，或先用「安装系统软件包」安装"; }
# fetch FILE URL...：已缓存直接用，否则依次尝试下载地址；输出本地路径
fetch() {
  _f="$DL_DIR/$1"; shift
  if [ -s "$_f" ]; then echo "[INFO] 使用缓存 $(basename "$_f")" >&2; printf '%s' "$_f"; return 0; fi
  _p="$_f.part.$$"
  for _u in "$@"; do
    echo "[INFO] 下载 $_u" >&2
    if curl -fSL --retry 2 --connect-timeout 15 -s -o "$_p" "$_u"; then mv -f "$_p" "$_f"; printf '%s' "$_f"; return 0; fi
    echo "[WARN] 下载失败，换下一个地址" >&2
  done
  rm -f "$_p"; return 1
}
cd "${FLOW_WORKSPACE:-.}"
`
