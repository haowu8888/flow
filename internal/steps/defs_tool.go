package steps

// 测试、工具、上传下载（构建物 / 对象存储）与网盘步骤。

func init() {
	defs = append(defs,
		&Kind{Kind: "node-unit-test", Category: "test", Label: "Node.js 单元测试", Image: "build-steps-node",
			Description: "执行测试命令并读取 Vitest / Jest 的 JSON 报告，按红线判定是否通过；输出用例统计供后续步骤引用",
			Inputs: []Input{
				{Key: "command", Label: "测试命令", Widget: "code", Default: "npx vitest run --reporter=json --outputFile=test-report.json"},
				{Key: "report_file", Label: "测试报告文件", Default: "test-report.json"},
				{Key: "redline", Label: "红线（每行「指标 比较符 阈值」）", Widget: "textarea", Default: "pass_rate >= 100",
					Description: "指标：pass_rate（通过率 %）、failures、passes、tests；比较符 >= > <= < ==；为空时以测试命令的退出码为准"},
			},
			Outputs: []string{"TEST_TOTAL", "TEST_PASSED", "TEST_FAILED", "TEST_PASS_RATE"},
			script: `need node
printf '%s\n' "$INPUT_COMMAND" > "${TMPDIR:-/tmp}/flow-cmd-$$.sh"
set +e; sh -e "${TMPDIR:-/tmp}/flow-cmd-$$.sh"; RC=$?; set -e
[ -f "$INPUT_REPORT_FILE" ] || { [ $RC = 0 ] && { warn "没有找到测试报告 $INPUT_REPORT_FILE"; exit 0; }; die "测试命令失败（退出码 $RC），且没有测试报告"; }
node - "$INPUT_REPORT_FILE" "$RC" <<'JS'
const fs = require('fs')
const [file, rc] = process.argv.slice(2)
const r = JSON.parse(fs.readFileSync(file, 'utf8'))
const tests = r.numTotalTests ?? 0, passes = r.numPassedTests ?? 0, failures = r.numFailedTests ?? 0
const v = { tests, passes, failures, pass_rate: tests ? Math.round((passes / tests) * 10000) / 100 : 100 }
const out = process.env.FLOW_OUTPUT
if (out) fs.appendFileSync(out, 'TEST_TOTAL=' + tests + '\nTEST_PASSED=' + passes + '\nTEST_FAILED=' + failures + '\nTEST_PASS_RATE=' + v.pass_rate + '\n')
console.log('[INFO] 用例 ' + tests + '，通过 ' + passes + '，失败 ' + failures + '，通过率 ' + v.pass_rate + '%')
const ops = { '>=': (a, b) => a >= b, '>': (a, b) => a > b, '<=': (a, b) => a <= b, '<': (a, b) => a < b, '==': (a, b) => a == b }
const rules = (process.env.INPUT_REDLINE || '').split('\n').map((l) => l.trim().split(/\s+/)).filter((r) => r.length === 3)
if (!rules.length) process.exit(Number(rc) === 0 ? 0 : 1)
let ok = true
for (const [m, op, raw] of rules) {
  if (!(m in v) || !ops[op]) { console.log('[WARN] 忽略无法识别的红线：' + [m, op, raw].join(' ')); continue }
  const hit = ops[op](v[m], Number(raw))
  ok = ok && hit
  console.log((hit ? '[INFO] ' : '[ERROR] ') + '红线 ' + m + ' ' + op + ' ' + raw + '：实际 ' + v[m] + (hit ? ' ✓' : ' ✗'))
}
process.exit(ok ? 0 : 1)
JS
ok "测试通过"
`},

		// ---- 工具 ----
		&Kind{Kind: "set-variable", Category: "tool", Label: "设置变量",
			Description: "设置任务输出变量（每行一个 KEY=VALUE，值可引用其他变量），后续步骤与下游任务可用；运行条件也能引用",
			Inputs: []Input{
				{Key: "vars", Label: "变量（每行一个 KEY=VALUE）", Widget: "textarea", Required: true, Placeholder: "VERSION=1.0.${FLOW_RUN_NUMBER}"},
			},
			script: `printf '%s\n' "$INPUT_VARS" | while IFS= read -r l; do
  case "$l" in ''|'#'*) continue ;; esac
  k="${l%%=*}"; v="${l#*=}"
  printf '%s' "$k" | grep -Eq '^[A-Za-z_][A-Za-z0-9_]*$' || die "无效的变量名：$k"
  set_output "$k" "$v"; info "$k=$v"
done
`},

		&Kind{Kind: "http-request", Category: "tool", Label: "HTTP 请求",
			Description: "发送 HTTP 请求（如调用部署接口、刷新 CDN），状态码不符时失败；可从 JSON 响应里取值作为输出",
			Inputs: []Input{
				{Key: "method", Label: "请求方法", Widget: "select", Default: "GET", Options: opts("GET", "GET", "POST", "POST", "PUT", "PUT", "PATCH", "PATCH", "DELETE", "DELETE")},
				{Key: "url", Label: "请求地址", Required: true},
				{Key: "headers", Label: "请求头（每行一个 Name: Value）", Widget: "textarea"},
				{Key: "body", Label: "请求体", Widget: "textarea"},
				{Key: "content_type", Label: "请求体类型", Default: "application/json"},
				{Key: "expect", Label: "期望的状态码", Default: "2xx", Description: "如 200、2xx，多个用逗号分隔"},
				{Key: "timeout", Label: "超时（秒）", Default: "30"},
				{Key: "retries", Label: "失败重试次数", Default: "0", Advanced: true},
				{Key: "insecure", Label: "不校验 HTTPS 证书", Widget: "checkbox", Advanced: true},
				{Key: "json_path", Label: "从 JSON 响应里取值的路径", Placeholder: "data.id（输出为 HTTP_VALUE）", Advanced: true},
			},
			Outputs: []string{"HTTP_STATUS", "HTTP_BODY", "HTTP_VALUE"},
			script: `need curl
OUT="${TMPDIR:-/tmp}/flow-http-$$"
set -- -sS -o "$OUT" -w '%{http_code}' -X "$INPUT_METHOD" --max-time "$INPUT_TIMEOUT" --retry "$INPUT_RETRIES"
truthy "$INPUT_INSECURE" && set -- "$@" -k
printf '%s\n' "$INPUT_HEADERS" > "$OUT.h"
while IFS= read -r h; do [ -n "$h" ] && set -- "$@" -H "$h"; done < "$OUT.h"
[ -n "$INPUT_BODY" ] && set -- "$@" -H "Content-Type: $INPUT_CONTENT_TYPE" --data-binary "$INPUT_BODY"
CODE=$(curl "$@" "$INPUT_URL") || die "请求失败"
info "HTTP $CODE"
head -c 2000 "$OUT"; echo
set_output HTTP_STATUS "$CODE"
set_output HTTP_BODY "$(head -c 4000 "$OUT" | tr '\n' ' ')"
if [ -n "$INPUT_JSON_PATH" ]; then
  V=$(python3 -c 'import json,sys
d=json.load(open(sys.argv[1]))
for k in sys.argv[2].split("."):
    d=d[int(k)] if isinstance(d,list) else d[k]
print(d if not isinstance(d,(dict,list)) else json.dumps(d,ensure_ascii=False))' "$OUT" "$INPUT_JSON_PATH" 2>/dev/null) || die "响应里没有 $INPUT_JSON_PATH"
  set_output HTTP_VALUE "$V"; info "HTTP_VALUE=$V"
fi
okc=0
for e in $(printf '%s' "$INPUT_EXPECT" | tr ',' ' '); do
  case "$e" in ?xx) [ "${CODE%??}" = "${e%xx}" ] && okc=1 ;; *) [ "$CODE" = "$e" ] && okc=1 ;; esac
done
[ $okc = 1 ] || die "状态码 $CODE 不是期望的 $INPUT_EXPECT"
ok "请求成功"
`},

		&Kind{Kind: "install-packages", Category: "tool", Label: "安装系统软件包", Image: "build-steps-linux",
			Description: "用系统包管理器安装软件包（如 make、cmake、libgtk-3-dev），自动识别 apt / apk / dnf / yum / brew",
			Inputs: []Input{
				{Key: "packages", Label: "软件包（空格分隔）", Required: true},
				{Key: "mirror", Label: "apt / apk 下载源", Widget: "select", Default: "tuna", Options: opts("tuna", "清华镜像", "aliyun", "阿里云", "default", "不修改")},
			},
			script: `SUDO=""; [ "$(id -u)" = 0 ] || SUDO="sudo"
H=mirrors.tuna.tsinghua.edu.cn; [ "$INPUT_MIRROR" = aliyun ] && H=mirrors.aliyun.com
if command -v apt-get >/dev/null 2>&1; then
  if [ "$INPUT_MIRROR" != default ]; then
    for f in /etc/apt/sources.list /etc/apt/sources.list.d/*.sources /etc/apt/sources.list.d/*.list; do
      [ -f "$f" ] && $SUDO sed -i -E "s#https?://(deb|archive|security)\.(debian|ubuntu)\.(org|com)#http://$H#g" "$f" || true
    done
  fi
  $SUDO apt-get update -q && DEBIAN_FRONTEND=noninteractive $SUDO apt-get install -y -q $INPUT_PACKAGES
elif command -v apk >/dev/null 2>&1; then
  [ "$INPUT_MIRROR" != default ] && $SUDO sed -i "s#dl-cdn.alpinelinux.org#$H#g" /etc/apk/repositories
  $SUDO apk add --no-cache $INPUT_PACKAGES
elif command -v dnf >/dev/null 2>&1; then $SUDO dnf install -y $INPUT_PACKAGES
elif command -v yum >/dev/null 2>&1; then $SUDO yum install -y $INPUT_PACKAGES
elif command -v brew >/dev/null 2>&1; then brew install $INPUT_PACKAGES
else die "没有识别到包管理器"; fi
ok "已安装 $INPUT_PACKAGES"
`},

		// ---- 构建物（Runner 直接执行）----
		&Kind{Kind: "artifact-upload", Category: "upload", Label: "构建物上传", Native: true, Connection: "oss", ConnectionOptional: true,
			Description: "把打包路径压缩为 zip 上传为构建物（同名构建物版本自动递增 v1、v2…），输出 ARTIFACT_<名称>，供下游下载或部署；选了对象存储连接时存到该 Bucket（如 Cloudflare R2），否则存在 Flow Server",
			Inputs: []Input{
				{Key: "name", Label: "构建物名称", Required: true, Placeholder: "app"},
				{Key: "paths", Label: "打包路径（每行一个，相对代码目录，支持通配符）", Widget: "textarea", Required: true, Default: "dist"},
				{Key: "version", Label: "制品版本", Placeholder: "留空自动递增"},
				{Key: "include_dir", Label: "制品中包含打包路径的目录", Widget: "checkbox"},
				{Key: "bucket", Label: "Bucket", Placeholder: "留空用服务连接的默认 Bucket", Description: "仅选了对象存储连接时有效", Advanced: true},
				{Key: "expire_days", Label: "保留天数", Placeholder: "留空沿用流水线设置", Description: "0 为永久保留；过期后连同存储里的文件一起删除", Advanced: true},
			},
			Outputs: []string{"ARTIFACT_<名称>", "ARTIFACT_<名称>_VERSION", "ARTIFACT_<名称>_URL"}},

		&Kind{Kind: "artifact-download", Category: "download", Label: "下载构建物", Native: true,
			Description: "下载上游任务上传的构建物并解压到指定目录",
			Inputs: []Input{
				{Key: "artifact", Label: "构建物名称", Required: true},
				{Key: "target_dir", Label: "解压到的目录（相对代码目录）", Default: "."},
			}},

		// ---- 对象存储 ----
		&Kind{Kind: "oss-upload", Category: "upload", Label: "OSS 上传", Connection: "oss",
			Description: "把代码目录里的文件或目录上传到对象存储（S3 兼容），目录会递归上传；配置了公开访问域名时输出 OSS_URL",
			Inputs: []Input{
				{Key: "bucket", Label: "Bucket 名称", Placeholder: "留空用服务连接里的默认 Bucket"},
				{Key: "source_path", Label: "本地文件或目录（相对代码目录）", Required: true, Default: "dist"},
				{Key: "object_path", Label: "上传到 Bucket 的路径", Required: true, Placeholder: "app/${FLOW_RUN_NUMBER}/", Description: "目录时作为前缀"},
			},
			Outputs: []string{"OSS_BUCKET", "OSS_OBJECT", "OSS_URL"},
			script: s3Helper + `S="$INPUT_SOURCE_PATH"; [ -e "$S" ] || die "$S 不存在"
P="${INPUT_OBJECT_PATH#/}"
if [ -d "$S" ]; then
  find "$S" -type f | while IFS= read -r f; do rel="${f#$S/}"; s3 PUT "$B" "${P%/}/$rel" "$f"; info "上传 ${P%/}/$rel"; done
  KEY="${P%/}/"
else
  case "$P" in */|'') KEY="$P$(basename "$S")" ;; *) KEY="$P" ;; esac
  s3 PUT "$B" "$KEY" "$S"; info "上传 $KEY"
fi
set_output OSS_BUCKET "$B"
set_output OSS_OBJECT "$KEY"
[ -n "${CONN_PUBLIC_DOMAIN:-}" ] && set_output OSS_URL "${CONN_PUBLIC_DOMAIN%/}/$KEY"
ok "上传完成"
`},

		&Kind{Kind: "oss-download", Category: "download", Label: "OSS 下载", Connection: "oss",
			Description: "从对象存储下载文件到代码目录（S3 兼容：阿里云 OSS / MinIO / AWS）",
			Inputs: []Input{
				{Key: "bucket", Label: "Bucket 名称", Placeholder: "留空用服务连接里的默认 Bucket"},
				{Key: "object_path", Label: "Bucket 内源文件路径", Required: true},
				{Key: "target_path", Label: "下载到的本地路径（相对代码目录）", Required: true},
			},
			script: s3Helper + `mkdir -p "$(dirname "$INPUT_TARGET_PATH")"
s3 GET "$B" "${INPUT_OBJECT_PATH#/}" "$INPUT_TARGET_PATH"
ok "已下载到 $INPUT_TARGET_PATH"
`},

		// ---- 网盘（Cloudreve v4）----
		&Kind{Kind: "cloudreve-upload", Category: "drive", Label: "网盘上传", Connection: "cloudreve",
			Description: "把代码目录里的文件或目录上传到 Cloudreve 网盘（目录递归上传），可生成分享链接；上传记录展示在部署历史",
			Inputs: []Input{
				{Key: "source", Label: "上传路径（每行一个，相对代码目录，支持通配符）", Widget: "textarea", Required: true, Default: "dist"},
				{Key: "target", Label: "网盘目录", Required: true, Default: "/builds/${FLOW_PIPELINE}"},
				{Key: "include_dir", Label: "保留目录本身", Widget: "checkbox", Description: "不勾选只上传目录里的内容；勾选后保留目录（如 dist/index.html）"},
				{Key: "overwrite", Label: "覆盖已有文件", Widget: "checkbox", Default: "true", Description: "同名文件作为新版本上传；不勾选时遇到同名文件报错"},
				{Key: "clean", Label: "上传前清空目标目录", Widget: "checkbox", Description: "先把目标目录移到网盘回收站再上传，目录里只剩这次的文件"},
				{Key: "share", Label: "生成分享链接", Widget: "checkbox", Description: "上传后为目标目录创建公开分享链接，输出 CLOUDREVE_SHARE_URL"},
			},
			Outputs: []string{"CLOUDREVE_DIR", "CLOUDREVE_FILES", "CLOUDREVE_SIZE", "CLOUDREVE_URL", "CLOUDREVE_SHARE_URL"},
			script: cloudreveHelper + `cr upload "$INPUT_SOURCE" "$INPUT_TARGET"
`},
		&Kind{Kind: "cloudreve-download", Category: "drive", Label: "网盘下载", Connection: "cloudreve",
			Description: "从 Cloudreve 网盘下载文件或整个目录到代码目录",
			Inputs: []Input{
				{Key: "source", Label: "网盘文件或目录", Required: true},
				{Key: "target", Label: "本地路径（相对代码目录）", Default: "."},
			},
			script: cloudreveHelper + `cr download "$INPUT_SOURCE" "$INPUT_TARGET"
`},
		&Kind{Kind: "cloudreve-delete", Category: "drive", Label: "网盘删除", Connection: "cloudreve",
			Description: "删除 Cloudreve 网盘上的文件或目录，默认移到回收站",
			Inputs: []Input{
				{Key: "paths", Label: "网盘路径（每行一个）", Widget: "textarea", Required: true},
				{Key: "skip_trash", Label: "彻底删除（不进回收站）", Widget: "checkbox"},
			},
			script: cloudreveHelper + `cr delete "$INPUT_PATHS" "$INPUT_SKIP_TRASH"
`},
		&Kind{Kind: "cloudreve-mkdir", Category: "drive", Label: "网盘新建目录", Connection: "cloudreve",
			Description: "在 Cloudreve 网盘上创建目录（多级目录一并创建，已存在不报错）",
			Inputs: []Input{
				{Key: "paths", Label: "网盘目录（每行一个）", Widget: "textarea", Required: true},
			},
			script: cloudreveHelper + `cr mkdir "$INPUT_PATHS"
`},
	)
}

// s3Helper 定义 s3 METHOD BUCKET KEY LOCAL：用 Python 做 SigV4 签名（不依赖 aws CLI）。
const s3Helper = `need python3
B="${INPUT_BUCKET:-${CONN_BUCKET:-}}"; [ -n "$B" ] || die "没有指定 Bucket"
s3() {
python3 - "$@" <<'PY'
import datetime, hashlib, hmac, os, sys, urllib.parse, urllib.request
method, bucket, key, local = sys.argv[1:5]
ep = os.environ["CONN_ENDPOINT"].rstrip("/")
region = os.environ.get("CONN_REGION") or "us-east-1"
ak, sk = os.environ["CONN_ACCESS_KEY_ID"], os.environ["CONN_ACCESS_KEY_SECRET"]
scheme, host = ep.split("://", 1)
path_style = os.environ.get("CONN_PATH_STYLE") == "true"
qkey = urllib.parse.quote(key, safe="/~")
if path_style:
    url, path = f"{scheme}://{host}/{bucket}/{qkey}", f"/{bucket}/{qkey}"
else:
    host = f"{bucket}.{host}"; url, path = f"{scheme}://{host}/{qkey}", f"/{qkey}"
data = open(local, "rb").read() if method == "PUT" else b""
payload = hashlib.sha256(data).hexdigest()
now = datetime.datetime.now(datetime.timezone.utc)
amz, day = now.strftime("%Y%m%dT%H%M%SZ"), now.strftime("%Y%m%d")
headers = {"host": host, "x-amz-content-sha256": payload, "x-amz-date": amz}
signed = ";".join(sorted(headers))
canon = "\n".join([method, path, "", "".join(f"{k}:{headers[k]}\n" for k in sorted(headers)), signed, payload])
scope = f"{day}/{region}/s3/aws4_request"
sts = "\n".join(["AWS4-HMAC-SHA256", amz, scope, hashlib.sha256(canon.encode()).hexdigest()])
k = ("AWS4" + sk).encode()
for part in (day, region, "s3", "aws4_request"):
    k = hmac.new(k, part.encode(), hashlib.sha256).digest()
sig = hmac.new(k, sts.encode(), hashlib.sha256).hexdigest()
headers["authorization"] = f"AWS4-HMAC-SHA256 Credential={ak}/{scope}, SignedHeaders={signed}, Signature={sig}"
req = urllib.request.Request(url, data=data if method == "PUT" else None, method=method, headers=headers)
try:
    with urllib.request.urlopen(req, timeout=300) as r:
        if method == "GET":
            open(local, "wb").write(r.read())
except urllib.error.HTTPError as e:
    sys.exit(f"[ERROR] {method} {key}: HTTP {e.code} {e.read()[:300].decode(errors='replace')}")
PY
}
`

// cloudreveHelper 定义 cr 命令：用账号密码换取令牌后调用 Cloudreve v4 接口。
const cloudreveHelper = `need python3
cr() {
python3 - "$@" <<'PY'
import json, os, sys, urllib.parse, urllib.request
base = os.environ["CONN_SERVER"].rstrip("/") + "/api/v4"
def call(method, path, body=None, raw=None, token=None):
    data = raw if raw is not None else (json.dumps(body).encode() if body is not None else None)
    req = urllib.request.Request(base + path, data=data, method=method)
    if body is not None: req.add_header("Content-Type", "application/json")
    if raw is not None: req.add_header("Content-Type", "application/octet-stream")
    if token: req.add_header("Authorization", "Bearer " + token)
    with urllib.request.urlopen(req, timeout=300) as r:
        out = r.read()
    d = json.loads(out or b"{}")
    if d.get("code", 0) != 0:
        sys.exit(f"[ERROR] {path}: {d.get('msg')}")
    return d.get("data")
tok = call("POST", "/session/token", {"email": os.environ["CONN_EMAIL"], "password": os.environ["CONN_PASSWORD"]})["token"]["access_token"]
def uri(p): return "cloudreve://my/" + "/".join(urllib.parse.quote(x) for x in p.strip("/").split("/") if x)
def mkdir(p): call("POST", "/file/create", {"uri": uri(p), "type": "folder"}, token=tok)
def lst(p):
    files, page = [], 0
    while True:
        d = call("GET", "/file?uri=" + urllib.parse.quote(uri(p)) + f"&page={page}&page_size=200", token=tok)
        files += d.get("files") or []
        if not d.get("pagination", {}).get("next_token") and len(d.get("files") or []) < 200: return files, d
        page += 1
def is_dir(f): return f.get("type") in (1, "folder")
def upload_file(local, remote_dir, overwrite=True, pol=None):
    size = os.path.getsize(local)
    name = os.path.basename(local)
    s = call("PUT", "/file/upload", {"uri": uri(remote_dir + "/" + name), "size": size, "policy_id": pol, "last_modified": int(os.path.getmtime(local) * 1000)}, token=tok)
    if s.get("upload_urls"):
        sys.exit("[ERROR] 网盘的存储策略是外部存储（直传），目前只支持本机存储策略")
    chunk = s.get("chunk_size") or size or 1
    with open(local, "rb") as f:
        i = 0
        while True:
            b = f.read(chunk)
            if not b and i > 0: break
            call("POST", f"/file/upload/{s['session_id']}/{i}", raw=b, token=tok)
            i += 1
            if not b: break
    print(f"[INFO] 上传 {remote_dir.rstrip('/')}/{name}（{size} 字节）")
    return size
def truthy(v): return (v or "").strip().lower() not in ("", "false", "0", "no", "n", "off", "否")
def output(k, v):
    if os.environ.get("FLOW_OUTPUT"):
        with open(os.environ["FLOW_OUTPUT"], "a") as f: f.write(f"{k}={v}\n")
def remove(paths, hard=False):
    body = json.dumps({"uris": [uri(p) for p in paths], "skip_soft_delete": hard, "unlink": False}).encode()
    req = urllib.request.Request(base + "/file", data=body, method="DELETE", headers={"Content-Type": "application/json", "Authorization": "Bearer " + tok})
    d = json.loads(urllib.request.urlopen(req, timeout=60).read() or b"{}")
    if d.get("code", 0) not in (0, 40016): sys.exit(f"[ERROR] 删除失败：{d.get('msg')}")
cmd = sys.argv[1]
if cmd == "upload":
    import glob
    patterns = [p.strip() for p in sys.argv[2].splitlines() if p.strip()]
    dst = "/" + sys.argv[3].strip().strip("/")
    keep, overwrite = truthy(os.environ.get("INPUT_INCLUDE_DIR")), truthy(os.environ.get("INPUT_OVERWRITE", "true"))
    if not patterns: sys.exit("[ERROR] 上传路径为空")
    files = []  # (本地文件, 网盘目录)
    for pat in patterns:
        hits = sorted(glob.glob(pat))
        if not hits: sys.exit(f"[ERROR] {pat} 没有匹配的文件")
        for src in hits:
            if os.path.isdir(src):
                top = dst + "/" + os.path.basename(os.path.normpath(src)) if keep else dst
                for root, _, names in os.walk(src):
                    rel = os.path.relpath(root, src)
                    for n in sorted(names):
                        files.append((os.path.join(root, n), top if rel == "." else top + "/" + rel.replace(os.sep, "/")))
            else:
                files.append((src, dst))
    if truthy(os.environ.get("INPUT_CLEAN")):
        remove([dst])
        print(f"[INFO] 已清空 {dst}（旧文件在网盘回收站）")
    pols, total = {}, 0
    for local, remote in files:
        if remote not in pols:
            mkdir(remote)
            existing, d = lst(remote)
            pols[remote] = ((d.get("storage_policy") or {}).get("id"), {f["name"] for f in existing})
        pol, names = pols[remote]
        if not overwrite and os.path.basename(local) in names:
            sys.exit(f"[ERROR] 网盘上已有 {remote}/{os.path.basename(local)}（未勾选覆盖已有文件）")
        total += upload_file(local, remote, overwrite, pol)
    server = os.environ["CONN_SERVER"].rstrip("/")
    output("CLOUDREVE_DIR", dst)
    output("CLOUDREVE_FILES", str(len(files)))
    output("CLOUDREVE_SIZE", str(total))
    output("CLOUDREVE_URL", server + "/home?path=" + urllib.parse.quote("cloudreve://my" + dst, safe=""))
    if truthy(os.environ.get("INPUT_SHARE")):
        try:
            link = call("PUT", "/share", {"uri": uri(dst), "is_private": False, "share_view": True, "permissions": {"anonymous": "BQ==", "everyone": "AQ=="}}, token=tok)
            output("CLOUDREVE_SHARE_URL", link)
            print(f"[INFO] 分享链接：{link}")
        except SystemExit as e:
            print(f"[WARN] 生成分享链接失败（文件已上传）：{e}")
    print(f"[SUCCESS] 已上传 {len(files)} 个文件到 {dst}")
elif cmd == "download":
    src, dst = sys.argv[2], sys.argv[3]
    def dl(remote, local):
        url = call("POST", "/file/url", {"uris": [uri(remote)], "download": True}, token=tok)["urls"][0]["url"]
        if url.startswith("/"): url = os.environ["CONN_SERVER"].rstrip("/") + url
        os.makedirs(os.path.dirname(local) or ".", exist_ok=True)
        urllib.request.urlretrieve(url, local)
        print(f"[INFO] 下载 {remote}")
    parent, name = os.path.split(src.rstrip("/"))
    files, _ = lst(parent or "/")
    hit = [f for f in files if f["name"] == name]
    if not hit: sys.exit(f"[ERROR] 网盘上没有 {src}")
    def walk(remote, local):
        fs, _ = lst(remote)
        for f in fs:
            r, l = remote.rstrip("/") + "/" + f["name"], os.path.join(local, f["name"])
            walk(r, l) if is_dir(f) else dl(r, l)
    if is_dir(hit[0]): walk(src, os.path.join(dst, name))
    else: dl(src, os.path.join(dst, name) if os.path.isdir(dst) or dst.endswith("/") or dst == "." else dst)
    print("[SUCCESS] 下载完成")
elif cmd == "delete":
    paths = [p.strip() for p in sys.argv[2].splitlines() if p.strip()]
    remove(paths, sys.argv[3] == "true")
    print("[SUCCESS] 已删除 " + "、".join(paths))
elif cmd == "mkdir":
    for p in [p.strip() for p in sys.argv[2].splitlines() if p.strip()]:
        mkdir(p); print(f"[INFO] 目录 {p}")
    print("[SUCCESS] 完成")
PY
}
`
