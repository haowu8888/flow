package steps

// 通知与发布步骤。

const notifyDefaults = "流水线 ${FLOW_PIPELINE} #${FLOW_RUN_NUMBER}：任务 ${FLOW_JOB} 执行到此步骤"

func init() {
	defs = append(defs,
		&Kind{Kind: "webhook-notify", Category: "notify", Label: "Webhook 通知",
			Description: "向任意地址发送 JSON 消息（title、content、pipeline、run 等字段）",
			Inputs: []Input{
				{Key: "url", Label: "Webhook 地址", Required: true, Description: "可引用密钥，如 ${HOOK_URL}"},
				{Key: "title", Label: "标题", Default: "Flow 通知"},
				{Key: "content", Label: "内容", Widget: "textarea", Default: notifyDefaults},
			},
			script: `need python3
python3 - <<'PY'
import json, os, urllib.request
e = os.environ
body = {"title": e["INPUT_TITLE"], "content": e["INPUT_CONTENT"], "pipeline": e.get("FLOW_PIPELINE"), "run": e.get("FLOW_RUN_NUMBER"), "job": e.get("FLOW_JOB")}
req = urllib.request.Request(e["INPUT_URL"], data=json.dumps(body, ensure_ascii=False).encode(), headers={"Content-Type": "application/json"})
urllib.request.urlopen(req, timeout=30).read()
print("[SUCCESS] 已发送")
PY
`},
		&Kind{Kind: "dingtalk-notify", Category: "notify", Label: "钉钉机器人",
			Description: "通过钉钉群机器人发送 Markdown 消息（支持加签）",
			Inputs: []Input{
				{Key: "url", Label: "Webhook 地址", Required: true, Description: "可引用密钥，如 ${DING_URL}"},
				{Key: "secret", Label: "加签密钥", Placeholder: "可选，如 ${DING_SECRET}"},
				{Key: "title", Label: "标题", Default: "Flow 通知"},
				{Key: "content", Label: "内容（Markdown）", Widget: "textarea", Default: notifyDefaults},
			},
			script: botScript("dingtalk")},
		&Kind{Kind: "feishu-notify", Category: "notify", Label: "飞书机器人",
			Description: "通过飞书群机器人发送消息（支持加签）",
			Inputs: []Input{
				{Key: "url", Label: "Webhook 地址", Required: true},
				{Key: "secret", Label: "加签密钥", Placeholder: "可选"},
				{Key: "title", Label: "标题", Default: "Flow 通知"},
				{Key: "content", Label: "内容", Widget: "textarea", Default: notifyDefaults},
			},
			script: botScript("feishu")},
		&Kind{Kind: "wecom-notify", Category: "notify", Label: "企业微信机器人", Connection: "wecom-bot",
			Description: "用企业微信机器人服务连接往群里发 Markdown 消息",
			Inputs: []Input{
				{Key: "title", Label: "标题", Default: "Flow 通知"},
				{Key: "content", Label: "内容（Markdown）", Widget: "textarea", Default: notifyDefaults},
			},
			script: botScript("wecom")},
		&Kind{Kind: "bark-notify", Category: "notify", Label: "Bark 推送", Connection: "bark",
			Description: "用 Bark 服务连接推送到 iPhone",
			Inputs: []Input{
				{Key: "title", Label: "标题", Default: "Flow 通知"},
				{Key: "content", Label: "内容", Widget: "textarea", Default: notifyDefaults},
				{Key: "url", Label: "点击跳转地址", Placeholder: "可选"},
			},
			script: botScript("bark")},
		&Kind{Kind: "telegram-notify", Category: "notify", Label: "Telegram 推送", Connection: "telegram",
			Description: "用 Telegram 服务连接往会话里发消息",
			Inputs: []Input{
				{Key: "title", Label: "标题", Default: "Flow 通知"},
				{Key: "content", Label: "内容", Widget: "textarea", Default: notifyDefaults},
			},
			script: botScript("telegram")},
		&Kind{Kind: "email-notify", Category: "notify", Label: "邮件通知", Connection: "email",
			Description: "通过邮件服务连接（SMTP）发邮件，可把文件作为附件",
			Inputs: []Input{
				{Key: "to", Label: "收件人（多个用逗号分隔）", Required: true},
				{Key: "subject", Label: "主题", Default: "Flow 通知：${FLOW_PIPELINE} #${FLOW_RUN_NUMBER}"},
				{Key: "body", Label: "正文", Widget: "textarea", Default: notifyDefaults},
				{Key: "attachments", Label: "附件路径（每行一个，相对代码目录）", Widget: "textarea", Advanced: true},
			},
			script: `need python3
python3 - <<'PY'
import os, smtplib, ssl, mimetypes
from email.message import EmailMessage
from email.utils import parseaddr
e = os.environ
m = EmailMessage()
sender = e.get("CONN_FROM") or e.get("CONN_USERNAME")
m["From"], m["To"], m["Subject"] = sender, e["INPUT_TO"], e["INPUT_SUBJECT"]
if e.get("CONN_REPLY_TO"): m["Reply-To"] = e["CONN_REPLY_TO"]
m.set_content(e["INPUT_BODY"])
for p in [x.strip() for x in e.get("INPUT_ATTACHMENTS", "").splitlines() if x.strip()]:
    ctype = (mimetypes.guess_type(p)[0] or "application/octet-stream").split("/")
    m.add_attachment(open(p, "rb").read(), maintype=ctype[0], subtype=ctype[1], filename=os.path.basename(p))
host, port = e["CONN_HOST"], int(e["CONN_PORT"])
s = smtplib.SMTP_SSL(host, port, timeout=30, context=ssl.create_default_context()) if e.get("CONN_TLS") == "true" else smtplib.SMTP(host, port, timeout=30)
if e.get("CONN_TLS") != "true":
    s.ehlo()
    if s.has_extn("starttls"): s.starttls(context=ssl.create_default_context())
if e.get("CONN_USERNAME"): s.login(e["CONN_USERNAME"], e.get("CONN_PASSWORD", ""))
s.send_message(m, from_addr=parseaddr(sender)[1])
s.quit()
print("[SUCCESS] 已发送到 " + e["INPUT_TO"])
PY
`},

		// ---- 发布 ----
		&Kind{Kind: "npm-publish", Category: "publish", Label: "NPM 发布", Connection: "npm", Image: "build-steps-node",
			Description: "用 NPM 仓库服务连接把包发布到仓库（打包 / 构建放在前面的步骤）",
			Inputs: []Input{
				{Key: "enabled", Label: "是否发布", Default: "是", Description: "可填 ${参数名} 引用运行参数；值为 否 / false / no / 0 / 空 时跳过发布"},
				{Key: "package_dir", Label: "包目录", Default: ".", Description: "package.json 所在目录，相对代码目录"},
				{Key: "dist_tag", Label: "发布标签", Default: "latest"},
				{Key: "access", Label: "访问级别", Widget: "select", Default: "", Options: opts("", "跟随 package.json", "public", "public（作用域包公开）", "restricted", "restricted（私有）"), Advanced: true},
				{Key: "dry_run", Label: "试运行（只打包检查，不发布）", Widget: "checkbox", Advanced: true},
			},
			Outputs: []string{"NPM_PACKAGE", "NPM_VERSION"},
			script: `truthy "$INPUT_ENABLED" || { info "是否发布 = ${INPUT_ENABLED:-空}，跳过"; exit 0; }
need npm; need node
cd "${INPUT_PACKAGE_DIR:-.}" || die "包目录不存在：$INPUT_PACKAGE_DIR"
[ -f package.json ] || die "$(pwd) 下没有 package.json"
NAME=$(node -p "require('./package.json').name"); VERSION=$(node -p "require('./package.json').version")
REG="${CONN_REGISTRY%/}/"; HOST="${REG#*://}"
RC="${TMPDIR:-/tmp}/flow-npmrc-$$"; trap 'rm -f "$RC"' EXIT
if [ -n "${CONN_TOKEN:-}" ]; then printf '//%s:_authToken=%s\n' "$HOST" "$CONN_TOKEN" > "$RC"
elif [ -n "${CONN_USERNAME:-}" ]; then printf '//%s:_auth=%s\n' "$HOST" "$(printf '%s:%s' "$CONN_USERNAME" "$CONN_PASSWORD" | base64 | tr -d '\n')" > "$RC"
else die "服务连接里没有令牌，也没有用户名 + 密码"; fi
export NPM_CONFIG_USERCONFIG="$RC"
npm --registry "$REG" whoami >/dev/null || die "登录仓库失败，请检查服务连接"
npm --registry "$REG" view "$NAME@$VERSION" version >/dev/null 2>&1 && die "仓库里已经有 $NAME@$VERSION，请先修改 package.json 的 version"
set -- publish --registry "$REG" --tag "${INPUT_DIST_TAG:-latest}"
[ -n "$INPUT_ACCESS" ] && set -- "$@" --access "$INPUT_ACCESS"
truthy "$INPUT_DRY_RUN" && set -- "$@" --dry-run
npm "$@"
set_output NPM_PACKAGE "$NAME"; set_output NPM_VERSION "$VERSION"
ok "$(truthy "$INPUT_DRY_RUN" && echo 试运行完成 || echo 已发布) $NAME@$VERSION"
`},
	)
}

// botScript 是机器人类通知的脚本（Python 发送，避免 shell 拼 JSON 的转义问题）。
func botScript(kind string) string {
	return `need python3
python3 - ` + kind + ` <<'PY'
import base64, hashlib, hmac, json, os, sys, time, urllib.parse, urllib.request
kind, e = sys.argv[1], os.environ
title, content = e.get("INPUT_TITLE", ""), e.get("INPUT_CONTENT", "")
def post(url, body):
    req = urllib.request.Request(url, data=json.dumps(body, ensure_ascii=False).encode(), headers={"Content-Type": "application/json"})
    out = json.loads(urllib.request.urlopen(req, timeout=30).read() or b"{}")
    code = out.get("errcode", out.get("code", out.get("StatusCode", 0)))
    if code not in (0, 200, None) and not out.get("ok", False):
        sys.exit(f"[ERROR] 发送失败：{out}")
if kind == "dingtalk":
    url = e["INPUT_URL"]
    if e.get("INPUT_SECRET"):
        ts = str(int(time.time() * 1000))
        sign = base64.b64encode(hmac.new(e["INPUT_SECRET"].encode(), f"{ts}\n{e['INPUT_SECRET']}".encode(), hashlib.sha256).digest()).decode()
        url += ("&" if "?" in url else "?") + f"timestamp={ts}&sign={urllib.parse.quote(sign)}"
    post(url, {"msgtype": "markdown", "markdown": {"title": title, "text": f"### {title}\n\n{content}"}})
elif kind == "feishu":
    body = {"msg_type": "text", "content": {"text": f"{title}\n{content}"}}
    if e.get("INPUT_SECRET"):
        ts = str(int(time.time()))
        body["timestamp"] = ts
        body["sign"] = base64.b64encode(hmac.new(f"{ts}\n{e['INPUT_SECRET']}".encode(), b"", hashlib.sha256).digest()).decode()
    post(e["INPUT_URL"], body)
elif kind == "wecom":
    post(e["CONN_WEBHOOK"], {"msgtype": "markdown", "markdown": {"content": f"**{title}**\n{content}"}})
elif kind == "bark":
    body = {"device_key": e["CONN_DEVICE_KEY"], "title": title, "body": content, "group": "Flow"}
    if e.get("INPUT_URL"): body["url"] = e["INPUT_URL"]
    post((e.get("CONN_SERVER") or "https://api.day.app").rstrip("/") + "/push", body)
elif kind == "telegram":
    base = (e.get("CONN_API_BASE") or "https://api.telegram.org").rstrip("/")
    post(f"{base}/bot{e['CONN_BOT_TOKEN']}/sendMessage", {"chat_id": e["CONN_CHAT_ID"], "text": f"{title}\n{content}"})
print("[SUCCESS] 已发送")
PY
`
}
