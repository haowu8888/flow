package steps

// 构建类步骤：执行命令、安装语言环境、按语言构建。

var npmMirrors = opts(
	"https://registry.npmmirror.com", "npmmirror（国内镜像）",
	"https://registry.npmjs.org", "npmjs 官方",
	"https://mirrors.cloud.tencent.com/npm/", "腾讯云",
)

var defs = []*Kind{
	{Kind: "run-command", Category: "build", Label: "执行命令", Description: "在任务的构建环境里执行命令，工作目录为代码目录",
		Inputs: []Input{
			{Key: "command", Label: "要执行的命令", Widget: "code", Required: true, Placeholder: "make build"},
			{Key: "shell", Label: "运行 Shell", Widget: "select", Default: "sh", Options: opts("sh", "sh", "bash", "bash"), Advanced: true},
		},
		script: `[ "$INPUT_SHELL" = bash ] && need bash
printf '%s\n' "$INPUT_COMMAND" > "${TMPDIR:-/tmp}/flow-cmd-$$.sh"
exec "$INPUT_SHELL" -e "${TMPDIR:-/tmp}/flow-cmd-$$.sh"
`},

	{Kind: "setup-node", Category: "build", Label: "安装 Node 环境", Image: "build-steps-linux",
		Description: "下载 Node.js 并加入 PATH，可选安装 pnpm / yarn / cnpm；后面的步骤可直接使用 node / npm",
		Inputs: []Input{
			{Key: "node_version", Label: "Node.js 版本", Default: "22", Description: "主版本（如 22）或完整版本（如 22.11.0）"},
			{Key: "package_manager", Label: "包管理器", Widget: "select", Default: "npm", Options: opts("npm", "npm", "pnpm", "pnpm", "yarn", "yarn", "cnpm", "cnpm")},
			{Key: "registry", Label: "npm 源", Widget: "select", Default: "https://registry.npmmirror.com", Options: npmMirrors},
		},
		Outputs: []string{"NODE_VERSION"},
		script: `need curl; need tar
V="$INPUT_NODE_VERSION"
case "$V" in
  *.*.*) ;;
  *) V=$(curl -fsSL --retry 2 "https://registry.npmmirror.com/-/binary/node/index.json" | tr '{' '\n' | grep "\"version\":\"v$V\." | head -1 | sed 's/.*"version":"v\([^"]*\)".*/\1/')
     [ -n "$V" ] || die "找不到 Node.js $INPUT_NODE_VERSION 的版本" ;;
esac
DIR="$FLOW_TOOLS/node/$V-$OS-$ARCH"
if [ ! -x "$DIR/bin/node" ]; then
  F=$(fetch "node-v$V-$OS-$ARCH.tar.gz" "https://registry.npmmirror.com/-/binary/node/v$V/node-v$V-$OS-$ARCH.tar.gz" "https://nodejs.org/dist/v$V/node-v$V-$OS-$ARCH.tar.gz") || die "下载 Node.js $V 失败"
  mkdir -p "$DIR.tmp" && tar -xzf "$F" -C "$DIR.tmp" --strip-components=1 && rm -rf "$DIR" && mv "$DIR.tmp" "$DIR"
fi
add_path "$DIR/bin"
npm config set registry "$INPUT_REGISTRY" >/dev/null
export_env npm_config_registry "$INPUT_REGISTRY"
case "$INPUT_PACKAGE_MANAGER" in
  pnpm|yarn|cnpm) info "安装 $INPUT_PACKAGE_MANAGER"; npm install -g "$INPUT_PACKAGE_MANAGER" >/dev/null ;;
esac
set_output NODE_VERSION "$V"
ok "Node.js $(node -v)，npm $(npm -v)"
`},

	{Kind: "setup-java", Category: "build", Label: "安装 Java", Image: "build-steps-linux",
		Description: "下载 JDK（Eclipse Temurin）和 Maven 并加入 PATH；后面的步骤可直接使用 java / mvn",
		Inputs: []Input{
			{Key: "jdk_version", Label: "JDK 版本", Widget: "select", Default: "17", Options: opts("8", "8", "11", "11", "17", "17", "21", "21")},
			{Key: "maven_version", Label: "Maven 版本", Default: "3.9.9", Advanced: true},
			{Key: "maven_mirror", Label: "Maven 使用阿里云镜像", Widget: "checkbox", Default: "true"},
		},
		script: `need curl; need tar
J="$INPUT_JDK_VERSION"; MA=$([ "$ARCH" = x64 ] && echo x64 || echo aarch64)
DIR="$FLOW_TOOLS/jdk/$J-$OS-$MA"
if [ ! -x "$DIR/bin/java" ]; then
  F=$(fetch "temurin-$J-$OS-$MA.tar.gz" \
    "https://mirrors.tuna.tsinghua.edu.cn/Adoptium/$J/jdk/$MA/$OS/$(curl -fsSL "https://mirrors.tuna.tsinghua.edu.cn/Adoptium/$J/jdk/$MA/$OS/" | grep -o 'OpenJDK[^"]*\.tar\.gz' | head -1)" \
    "https://api.adoptium.net/v3/binary/latest/$J/ga/$OS/$MA/jdk/hotspot/normal/eclipse") || die "下载 JDK $J 失败"
  mkdir -p "$DIR.tmp" && tar -xzf "$F" -C "$DIR.tmp" --strip-components=1 && rm -rf "$DIR" && mv "$DIR.tmp" "$DIR"
fi
export_env JAVA_HOME "$DIR"
add_path "$DIR/bin"
M="$INPUT_MAVEN_VERSION"; MD="$FLOW_TOOLS/maven/$M"
if [ ! -x "$MD/bin/mvn" ]; then
  F=$(fetch "maven-$M.tar.gz" "https://mirrors.aliyun.com/apache/maven/maven-3/$M/binaries/apache-maven-$M-bin.tar.gz" "https://archive.apache.org/dist/maven/maven-3/$M/binaries/apache-maven-$M-bin.tar.gz") || die "下载 Maven $M 失败"
  mkdir -p "$MD.tmp" && tar -xzf "$F" -C "$MD.tmp" --strip-components=1 && rm -rf "$MD" && mv "$MD.tmp" "$MD"
fi
add_path "$MD/bin"
if truthy "$INPUT_MAVEN_MIRROR"; then
  mkdir -p "$HOME/.m2"
  [ -f "$HOME/.m2/settings.xml" ] || cat > "$HOME/.m2/settings.xml" <<'XML'
<settings><mirrors><mirror><id>aliyun</id><mirrorOf>central</mirrorOf><url>https://maven.aliyun.com/repository/public</url></mirror></mirrors></settings>
XML
fi
ok "$(java -version 2>&1 | head -1)；$(mvn -v | head -1)"
`},

	{Kind: "setup-python", Category: "build", Label: "安装 Python", Image: "build-steps-linux",
		Description: "下载 Python（独立发行版，带 pip）并加入 PATH；后面的步骤可直接使用 python / pip",
		Inputs: []Input{
			{Key: "python_version", Label: "Python 版本", Widget: "select", Default: "3.12", Options: opts("3.9", "3.9", "3.10", "3.10", "3.11", "3.11", "3.12", "3.12", "3.13", "3.13")},
			{Key: "pip_index", Label: "pip 源", Default: "https://pypi.tuna.tsinghua.edu.cn/simple"},
		},
		script: `need curl; need tar
PV="$INPUT_PYTHON_VERSION"; PA=$([ "$ARCH" = x64 ] && echo x86_64 || echo aarch64)
POS=$([ "$OS" = darwin ] && echo apple-darwin || echo unknown-linux-gnu)
DIR="$FLOW_TOOLS/python/$PV-$OS-$PA"
if [ ! -x "$DIR/bin/python3" ]; then
  REL=20241016
  FULL=$(curl -fsSL "https://registry.npmmirror.com/-/binary/python-build-standalone/$REL/" | grep -o "cpython-$PV\.[0-9]*+$REL-$PA-$POS-install_only.tar.gz" | head -1)
  [ -n "$FULL" ] || die "找不到 Python $PV 的发行包"
  F=$(fetch "$FULL" "https://registry.npmmirror.com/-/binary/python-build-standalone/$REL/$FULL" "https://github.com/indygreg/python-build-standalone/releases/download/$REL/$FULL") || die "下载 Python 失败"
  mkdir -p "$DIR.tmp" && tar -xzf "$F" -C "$DIR.tmp" --strip-components=1 && rm -rf "$DIR" && mv "$DIR.tmp" "$DIR"
fi
add_path "$DIR/bin"
export_env PIP_INDEX_URL "$INPUT_PIP_INDEX"
ln -sf python3 "$DIR/bin/python" 2>/dev/null || true
ok "$(python3 --version)，$(python3 -m pip --version | cut -d' ' -f1-2)"
`},

	{Kind: "setup-go", Category: "build", Label: "安装 Go", Image: "build-steps-linux",
		Description: "下载官方 Go 并加入 PATH；后面的步骤可直接使用 go",
		Inputs: []Input{
			{Key: "go_version", Label: "Go 版本", Default: "1.23.4"},
			{Key: "goproxy", Label: "GOPROXY", Default: "https://goproxy.cn,direct"},
			{Key: "gopath", Label: "GOPATH", Placeholder: "默认 $FLOW_TOOLS/gopath（跨任务复用模块缓存）", Advanced: true},
		},
		script: `need curl; need tar
GV="$INPUT_GO_VERSION"; DIR="$FLOW_TOOLS/go/$GV-$OS-$GOARCH"
if [ ! -x "$DIR/bin/go" ]; then
  F=$(fetch "go$GV.$OS-$GOARCH.tar.gz" "https://mirrors.aliyun.com/golang/go$GV.$OS-$GOARCH.tar.gz" "https://go.dev/dl/go$GV.$OS-$GOARCH.tar.gz") || die "下载 Go $GV 失败"
  mkdir -p "$DIR.tmp" && tar -xzf "$F" -C "$DIR.tmp" --strip-components=1 && rm -rf "$DIR" && mv "$DIR.tmp" "$DIR"
fi
add_path "$DIR/bin"
export_env GOPROXY "$INPUT_GOPROXY"
export_env GOTOOLCHAIN local
export_env GOPATH "${INPUT_GOPATH:-$FLOW_TOOLS/gopath}"
add_path "${INPUT_GOPATH:-$FLOW_TOOLS/gopath}/bin"
ok "$(go version)"
`},

	{Kind: "setup-rust", Category: "build", Label: "安装 Rust", Image: "build-steps-linux",
		Description: "用 rustup 安装 Rust 工具链（rustc / cargo）并加入 PATH，可添加编译目标和组件",
		Inputs: []Input{
			{Key: "toolchain", Label: "工具链", Default: "stable"},
			{Key: "targets", Label: "编译目标", Placeholder: "空格分隔，如 wasm32-unknown-unknown"},
			{Key: "components", Label: "组件", Placeholder: "空格分隔，如 clippy rustfmt"},
			{Key: "mirror", Label: "下载源", Widget: "select", Default: "rsproxy", Options: opts("rsproxy", "字节 rsproxy（国内）", "official", "官方")},
		},
		script: `need curl
if [ "$INPUT_MIRROR" = rsproxy ]; then
  export_env RUSTUP_DIST_SERVER https://rsproxy.cn; export_env RUSTUP_UPDATE_ROOT https://rsproxy.cn/rustup
fi
export_env RUSTUP_HOME "$FLOW_TOOLS/rustup"; export_env CARGO_HOME "$FLOW_TOOLS/cargo"
if [ ! -x "$CARGO_HOME/bin/rustup" ]; then
  U=$([ "$INPUT_MIRROR" = rsproxy ] && echo https://rsproxy.cn/rustup-init.sh || echo https://sh.rustup.rs)
  curl -fsSL "$U" | sh -s -- -y --no-modify-path --default-toolchain none
fi
add_path "$CARGO_HOME/bin"
rustup toolchain install "$INPUT_TOOLCHAIN" --profile minimal
rustup default "$INPUT_TOOLCHAIN"
[ -n "$INPUT_TARGETS" ] && rustup target add $INPUT_TARGETS
[ -n "$INPUT_COMPONENTS" ] && rustup component add $INPUT_COMPONENTS
ok "$(rustc --version)"
`},

	{Kind: "setup-platformio", Category: "build", Label: "安装 PlatformIO", Image: "build-steps-linux",
		Description: "安装 PlatformIO Core 并加入 PATH，用于编译 ESP32 / Arduino 等嵌入式固件；后面的步骤可直接使用 pio",
		Inputs: []Input{
			{Key: "pip_index", Label: "pip 源", Default: "https://pypi.tuna.tsinghua.edu.cn/simple"},
		},
		script: `need python3
VENV="$FLOW_TOOLS/platformio"
[ -x "$VENV/bin/pio" ] || { python3 -m venv "$VENV" && "$VENV/bin/pip" install -q -i "$INPUT_PIP_INDEX" platformio; }
add_path "$VENV/bin"
export_env PLATFORMIO_CORE_DIR "$FLOW_TOOLS/platformio-core"
ok "$(pio --version)"
`},

	{Kind: "setup-android", Category: "build", Label: "安装 Android 构建工具", Image: "build-steps-linux",
		Description: "下载 Android 命令行工具并接受许可，设置 ANDROID_HOME；需要先「安装 Java」（JDK 17）",
		Inputs: []Input{
			{Key: "packages", Label: "预装的 SDK 包", Default: "platform-tools platforms;android-34 build-tools;34.0.0", Description: "空格分隔，传给 sdkmanager"},
			{Key: "cmdline_tools", Label: "命令行工具 build 号", Default: "11076708", Advanced: true},
		},
		script: `need curl; need unzip; need java
SDK="$FLOW_TOOLS/android-sdk"
if [ ! -x "$SDK/cmdline-tools/latest/bin/sdkmanager" ]; then
  F=$(fetch "commandlinetools-$INPUT_CMDLINE_TOOLS.zip" "https://dl.google.com/android/repository/commandlinetools-linux-${INPUT_CMDLINE_TOOLS}_latest.zip") || die "下载 Android 命令行工具失败"
  rm -rf "$SDK/cmdline-tools" && mkdir -p "$SDK/cmdline-tools" && unzip -q "$F" -d "$SDK/cmdline-tools" && mv "$SDK/cmdline-tools/cmdline-tools" "$SDK/cmdline-tools/latest"
fi
export_env ANDROID_HOME "$SDK"; export_env ANDROID_SDK_ROOT "$SDK"
add_path "$SDK/cmdline-tools/latest/bin"; add_path "$SDK/platform-tools"
yes | sdkmanager --licenses >/dev/null 2>&1 || true
# shellcheck disable=SC2086
sdkmanager $INPUT_PACKAGES
ok "Android SDK 就绪：$SDK"
`},

	{Kind: "setup-flutter", Category: "build", Label: "安装 Flutter", Image: "build-steps-linux",
		Description: "下载 Flutter SDK 并加入 PATH；打 Android 包时前面再加「安装 Java」和「安装 Android 构建工具」",
		Inputs: []Input{
			{Key: "version", Label: "Flutter 版本", Default: "3.24.5"},
			{Key: "mirror", Label: "下载源", Widget: "select", Default: "cn", Options: opts("cn", "flutter-io.cn（国内）", "official", "官方")},
		},
		script: `need curl; need tar; need git
V="$INPUT_VERSION"; DIR="$FLOW_TOOLS/flutter/$V"
BASE=$([ "$INPUT_MIRROR" = cn ] && echo https://storage.flutter-io.cn || echo https://storage.googleapis.com)
if [ "$INPUT_MIRROR" = cn ]; then export_env PUB_HOSTED_URL https://pub.flutter-io.cn; export_env FLUTTER_STORAGE_BASE_URL https://storage.flutter-io.cn; fi
if [ ! -x "$DIR/bin/flutter" ]; then
  F=$(fetch "flutter_linux_$V-stable.tar.xz" "$BASE/flutter_infra_release/releases/stable/linux/flutter_linux_$V-stable.tar.xz") || die "下载 Flutter $V 失败"
  mkdir -p "$DIR.tmp" && tar -xJf "$F" -C "$DIR.tmp" --strip-components=1 && rm -rf "$DIR" && mv "$DIR.tmp" "$DIR"
fi
git config --global --add safe.directory "$DIR" || true
add_path "$DIR/bin"
flutter --version
`},

	{Kind: "node-build", Category: "build", Label: "Node.js 构建", Image: "build-steps-node",
		Description: "设置 npm 源后执行构建命令（构建环境需带 Node.js，或前面加「安装 Node 环境」）",
		Inputs: []Input{
			{Key: "npm_registry", Label: "npm 源", Widget: "select", Default: "https://registry.npmmirror.com", Options: npmMirrors},
			{Key: "command", Label: "构建命令", Widget: "code", Default: "npm ci\nnpm run build"},
		},
		script: `need node
export npm_config_registry="$INPUT_NPM_REGISTRY"
info "node $(node -v)，npm 源 $INPUT_NPM_REGISTRY"
printf '%s\n' "$INPUT_COMMAND" > "${TMPDIR:-/tmp}/flow-cmd-$$.sh"
sh -e "${TMPDIR:-/tmp}/flow-cmd-$$.sh"
`},

	{Kind: "java-build", Category: "build", Label: "Java 构建（Maven）", Image: "build-steps-java",
		Description: "用 Maven 构建 Java 项目（构建环境需带 JDK 与 Maven，或前面加「安装 Java」）",
		Inputs: []Input{
			{Key: "command", Label: "构建命令", Widget: "code", Default: "mvn -B -DskipTests clean package"},
		},
		script: `need mvn
printf '%s\n' "$INPUT_COMMAND" > "${TMPDIR:-/tmp}/flow-cmd-$$.sh"
sh -e "${TMPDIR:-/tmp}/flow-cmd-$$.sh"
`},

	{Kind: "python-build", Category: "build", Label: "Python 构建", Image: "build-steps-python",
		Description: "安装依赖并构建 Python 项目（构建环境需带 Python，或前面加「安装 Python」）",
		Inputs: []Input{
			{Key: "command", Label: "构建命令", Widget: "code", Default: "pip install -r requirements.txt\npython -m build"},
		},
		script: `need python3
printf '%s\n' "$INPUT_COMMAND" > "${TMPDIR:-/tmp}/flow-cmd-$$.sh"
sh -e "${TMPDIR:-/tmp}/flow-cmd-$$.sh"
`},

	{Kind: "go-build", Category: "build", Label: "Go 构建",
		Description: "编译 Go 项目（构建环境需带 Go 工具链，如 golang:1.23，或前面加「安装 Go」）",
		Inputs: []Input{
			{Key: "command", Label: "构建命令", Widget: "code", Default: "go build -o bin/ ./..."},
		},
		script: `need go
info "$(go version)"
printf '%s\n' "$INPUT_COMMAND" > "${TMPDIR:-/tmp}/flow-cmd-$$.sh"
sh -e "${TMPDIR:-/tmp}/flow-cmd-$$.sh"
`},
}
