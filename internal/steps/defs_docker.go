package steps

// Docker 与代码类步骤。

func init() {
	defs = append(defs,
		&Kind{Kind: "docker-login", Category: "docker", Label: "Docker 登录", Connection: "registry", Image: "build-steps-image-build",
			Description: "用服务连接里的账号登录镜像仓库，同一任务后续步骤可直接推送",
			Inputs: []Input{
				{Key: "registry", Label: "镜像仓库地址", Required: true, Placeholder: "registry.cn-hangzhou.aliyuncs.com"},
			},
			script: `need docker
[ -n "${CONN_USERNAME:-}" ] || die "请选择镜像仓库服务连接"
printf '%s' "$CONN_PASSWORD" | docker login -u "$CONN_USERNAME" --password-stdin "$INPUT_REGISTRY"
ok "已登录 $INPUT_REGISTRY"
`},

		&Kind{Kind: "docker-build", Category: "docker", Label: "构建镜像", Image: "build-steps-image-build",
			Description: "用 Dockerfile 构建镜像（不推送），输出镜像地址给后面的运行 / 推送 / 打标签步骤",
			Inputs: []Input{
				{Key: "image", Label: "镜像名称（不含 tag）", Required: true, Placeholder: "registry.example.com/team/app"},
				{Key: "tag", Label: "标签", Default: "${FLOW_RUN_NUMBER}", Description: "支持引用变量，如 ${FLOW_RUN_NUMBER}、${FLOW_COMMIT_SHORT}"},
				{Key: "dockerfile", Label: "Dockerfile 路径", Default: "Dockerfile"},
				{Key: "context", Label: "构建上下文", Default: "."},
				{Key: "platform", Label: "目标平台", Placeholder: "linux/amd64", Advanced: true},
				{Key: "no_cache", Label: "不使用缓存", Widget: "checkbox", Advanced: true},
				{Key: "build_args", Label: "build-arg（每行一个 KEY=VALUE）", Widget: "textarea", Advanced: true},
				{Key: "extra_args", Label: "更多 docker build 参数", Advanced: true},
			},
			Outputs: []string{"IMAGE", "IMAGE_TAG"},
			script: `need docker
REF="$INPUT_IMAGE:${INPUT_TAG:-latest}"
set -- build -f "$INPUT_DOCKERFILE" -t "$REF"
[ -n "$INPUT_PLATFORM" ] && set -- "$@" --platform "$INPUT_PLATFORM"
truthy "$INPUT_NO_CACHE" && set -- "$@" --no-cache
printf '%s\n' "$INPUT_BUILD_ARGS" | while IFS= read -r l; do [ -n "$l" ] && printf '%s\n' "$l"; done > "${TMPDIR:-/tmp}/flow-ba-$$"
while IFS= read -r l; do set -- "$@" --build-arg "$l"; done < "${TMPDIR:-/tmp}/flow-ba-$$"
# shellcheck disable=SC2086
docker "$@" $INPUT_EXTRA_ARGS "$INPUT_CONTEXT"
set_output IMAGE "$REF"; set_output IMAGE_TAG "${INPUT_TAG:-latest}"
ok "已构建 $REF"
`},

		&Kind{Kind: "docker-lint", Category: "docker", Label: "Dockerfile 检查", Image: "build-steps-image-build",
			Description: "用 hadolint 检查 Dockerfile 的写法问题（如未固定版本、缓存未清理），达到失败级别时步骤失败",
			Inputs: []Input{
				{Key: "dockerfile", Label: "Dockerfile 路径", Default: "Dockerfile"},
				{Key: "threshold", Label: "失败级别", Widget: "select", Default: "error", Options: opts("error", "error", "warning", "warning", "info", "info", "style", "style")},
				{Key: "ignore", Label: "忽略的规则", Placeholder: "空格分隔，如 DL3008 DL3018"},
			},
			script: `set -- --failure-threshold "$INPUT_THRESHOLD"
for r in $INPUT_IGNORE; do set -- "$@" --ignore "$r"; done
if command -v hadolint >/dev/null 2>&1; then hadolint "$@" "$INPUT_DOCKERFILE"
else need docker; docker run --rm -i hadolint/hadolint hadolint "$@" - < "$INPUT_DOCKERFILE"; fi
ok "Dockerfile 检查通过"
`},

		&Kind{Kind: "docker-run", Category: "docker", Label: "运行镜像", Image: "build-steps-image-build",
			Description: "用 docker run 运行一次镜像（如冒烟测试、在镜像里执行命令），容器退出码非 0 时失败",
			Inputs: []Input{
				{Key: "image", Label: "镜像地址", Default: "${IMAGE}", Required: true},
				{Key: "command", Label: "运行命令", Placeholder: "留空用镜像默认命令"},
				{Key: "env", Label: "环境变量（每行一个 KEY=VALUE）", Widget: "textarea"},
				{Key: "timeout", Label: "超时（秒）", Default: "600"},
				{Key: "pull", Label: "运行前拉取镜像", Widget: "checkbox", Advanced: true},
				{Key: "extra_args", Label: "更多 docker run 参数", Advanced: true},
			},
			script: `need docker
truthy "$INPUT_PULL" && docker pull "$INPUT_IMAGE"
NAME="flow-run-$$"
set -- run --rm --name "$NAME"
printf '%s\n' "$INPUT_ENV" > "${TMPDIR:-/tmp}/flow-env-$$"
while IFS= read -r l; do [ -n "$l" ] && set -- "$@" -e "$l"; done < "${TMPDIR:-/tmp}/flow-env-$$"
# shellcheck disable=SC2086
set -- "$@" $INPUT_EXTRA_ARGS "$INPUT_IMAGE"
[ -n "$INPUT_COMMAND" ] && set -- "$@" sh -c "$INPUT_COMMAND"
if command -v timeout >/dev/null 2>&1; then timeout "$INPUT_TIMEOUT" docker "$@" || { rc=$?; docker rm -f "$NAME" >/dev/null 2>&1; exit $rc; }
else docker "$@"; fi
ok "容器正常退出"
`},

		&Kind{Kind: "docker-push", Category: "docker", Label: "推送镜像", Image: "build-steps-image-build",
			Description: "把本机构建好的镜像推送到镜像仓库，推送前需要 Docker 登录",
			Inputs: []Input{
				{Key: "images", Label: "镜像地址（每行一个）", Widget: "textarea", Default: "${IMAGE}", Required: true},
			},
			script: `need docker
printf '%s\n' "$INPUT_IMAGES" | while IFS= read -r img; do
  [ -n "$img" ] || continue
  info "推送 $img"; docker push "$img"
done
ok "推送完成"
`},

		&Kind{Kind: "docker-tag", Category: "docker", Label: "镜像打标签", Image: "build-steps-image-build",
			Description: "给镜像加新的标签（如 latest、版本号），可同时推送到镜像仓库",
			Inputs: []Input{
				{Key: "source", Label: "源镜像", Default: "${IMAGE}", Required: true},
				{Key: "target", Label: "目标镜像名称（不含 tag）", Placeholder: "留空与源镜像相同"},
				{Key: "tags", Label: "新标签（空格分隔）", Default: "latest", Required: true},
				{Key: "push", Label: "打完标签后推送", Widget: "checkbox", Default: "true"},
			},
			script: `need docker
T="${INPUT_TARGET:-${INPUT_SOURCE%:*}}"
for t in $INPUT_TAGS; do
  docker tag "$INPUT_SOURCE" "$T:$t"; info "已打标签 $T:$t"
  truthy "$INPUT_PUSH" && docker push "$T:$t"
done
ok "完成"
`},

		&Kind{Kind: "image-build-push", Category: "docker", Label: "镜像构建并推送", Image: "build-steps-image-build",
			Description: "用代码目录里的 Dockerfile 构建镜像并推送到镜像仓库，推送前需要 Docker 登录",
			Inputs: []Input{
				{Key: "image", Label: "镜像地址（不含 tag）", Required: true},
				{Key: "tag", Label: "标签", Default: "${FLOW_RUN_NUMBER}"},
				{Key: "dockerfile", Label: "Dockerfile 路径", Default: "Dockerfile"},
				{Key: "context", Label: "构建上下文", Default: "."},
				{Key: "platform", Label: "目标平台", Placeholder: "linux/amd64,linux/arm64（多平台需 buildx）", Advanced: true},
				{Key: "build_args", Label: "build-arg（每行一个 KEY=VALUE）", Widget: "textarea", Advanced: true},
				{Key: "extra_args", Label: "更多 docker build 参数", Advanced: true},
			},
			Outputs: []string{"IMAGE", "IMAGE_TAG"},
			script: `need docker
REF="$INPUT_IMAGE:${INPUT_TAG:-latest}"
printf '%s\n' "$INPUT_BUILD_ARGS" > "${TMPDIR:-/tmp}/flow-ba-$$"
case "$INPUT_PLATFORM" in
  *,*) set -- buildx build --push --platform "$INPUT_PLATFORM" -f "$INPUT_DOCKERFILE" -t "$REF" ;;
  *)   set -- build -f "$INPUT_DOCKERFILE" -t "$REF"; [ -n "$INPUT_PLATFORM" ] && set -- "$@" --platform "$INPUT_PLATFORM" ;;
esac
while IFS= read -r l; do [ -n "$l" ] && set -- "$@" --build-arg "$l"; done < "${TMPDIR:-/tmp}/flow-ba-$$"
# shellcheck disable=SC2086
docker "$@" $INPUT_EXTRA_ARGS "$INPUT_CONTEXT"
case "$INPUT_PLATFORM" in *,*) ;; *) docker push "$REF" ;; esac
set_output IMAGE "$REF"; set_output IMAGE_TAG "${INPUT_TAG:-latest}"
ok "已推送 $REF"
`},

		// ---- 代码 ----
		&Kind{Kind: "git-tag", Category: "code", Label: "Git 标签", Connection: "git",
			Description: "给当前提交打 tag 并推送到远端",
			Inputs: []Input{
				{Key: "tag", Label: "标签名", Default: "v${FLOW_RUN_NUMBER}", Required: true},
				{Key: "message", Label: "标签说明", Placeholder: "留空为轻量标签"},
				{Key: "force", Label: "已存在时覆盖", Widget: "checkbox"},
			},
			script: gitAuth + `set -- tag
truthy "$INPUT_FORCE" && set -- "$@" -f
if [ -n "$INPUT_MESSAGE" ]; then git "$@" -a "$INPUT_TAG" -m "$INPUT_MESSAGE"; else git "$@" "$INPUT_TAG"; fi
set -- push "$REMOTE" "refs/tags/$INPUT_TAG"
truthy "$INPUT_FORCE" && set -- "$@" -f
git "$@"
ok "已推送标签 $INPUT_TAG"
`},

		&Kind{Kind: "git-merge", Category: "code", Label: "Git 代码合并", Connection: "git",
			Description: "把当前提交合并到目标分支并推送",
			Inputs: []Input{
				{Key: "target_branch", Label: "目标分支", Required: true, Placeholder: "main"},
				{Key: "strategy", Label: "合并方式", Widget: "select", Default: "merge", Options: opts("merge", "普通合并（--no-ff）", "ff-only", "仅快进")},
			},
			script: gitAuth + `CUR=$(git rev-parse HEAD)
git fetch "$REMOTE" "$INPUT_TARGET_BRANCH"
git checkout -B "$INPUT_TARGET_BRANCH" FETCH_HEAD
if [ "$INPUT_STRATEGY" = ff-only ]; then git merge --ff-only "$CUR"; else git merge --no-ff -m "Merge $CUR into $INPUT_TARGET_BRANCH" "$CUR"; fi
git push "$REMOTE" "HEAD:refs/heads/$INPUT_TARGET_BRANCH"
ok "已合并到 $INPUT_TARGET_BRANCH"
`},

		&Kind{Kind: "git-push", Category: "code", Label: "Git 推送到远端仓库", Connection: "git",
			Description: "把当前代码推送到另一个 Git 仓库的指定分支（如同步到私服）",
			Inputs: []Input{
				{Key: "remote_url", Label: "远端仓库地址（https）", Required: true},
				{Key: "branch", Label: "推送到远端的分支", Required: true, Default: "main"},
				{Key: "source_ref", Label: "要推送的提交 / 分支", Placeholder: "留空为当前提交"},
				{Key: "force", Label: "强制推送（覆盖远端分支）", Widget: "checkbox"},
				{Key: "tags", Label: "同时推送标签", Widget: "checkbox"},
			},
			script: `need git
U="$INPUT_REMOTE_URL"
case "$U" in https://*) U="https://$(printf '%s' "${CONN_USERNAME:-git}" | sed 's/@/%40/g'):${CONN_TOKEN}@${U#https://}" ;; *) die "远端仓库地址必须是 https" ;; esac
set -- push "$U" "${INPUT_SOURCE_REF:-HEAD}:refs/heads/$INPUT_BRANCH"
truthy "$INPUT_FORCE" && set -- "$@" --force
git "$@"
truthy "$INPUT_TAGS" && git push "$U" --tags
ok "已推送到 $INPUT_REMOTE_URL $INPUT_BRANCH"
`},

		&Kind{Kind: "git-delete-branch", Category: "code", Label: "删除 Git 分支", Connection: "git",
			Description: "删除远端分支",
			Inputs: []Input{
				{Key: "branch", Label: "要删除的分支", Required: true},
			},
			script: gitAuth + `git push "$REMOTE" --delete "$INPUT_BRANCH"
ok "已删除远端分支 $INPUT_BRANCH"
`},

		&Kind{Kind: "git-behind-check", Category: "code", Label: "检测是否落后于指定 Git 分支",
			Description: "当前提交落后目标分支时让流水线失败，避免基于旧代码发布",
			Inputs: []Input{
				{Key: "base_branch", Label: "对比的分支", Default: "main", Required: true},
			},
			script: `need git
git fetch origin "$INPUT_BASE_BRANCH" >/dev/null 2>&1 || git fetch --unshallow origin "$INPUT_BASE_BRANCH" || die "拉取 $INPUT_BASE_BRANCH 失败"
N=$(git rev-list --count HEAD..FETCH_HEAD)
[ "$N" = 0 ] || die "当前提交落后 $INPUT_BASE_BRANCH $N 个提交，请先合并最新代码"
ok "没有落后于 $INPUT_BASE_BRANCH"
`},
	)
}

// gitAuth 用服务连接的账号令牌改写 origin 的地址，得到可推送的 REMOTE。
const gitAuth = `need git
[ -n "${CONN_TOKEN:-}" ] || die "请选择 Git 服务连接"
O=$(git remote get-url origin 2>/dev/null) || die "代码目录不是 Git 仓库（任务需要下载流水线源）"
O=$(printf '%s' "$O" | sed -E 's#^(https?://)[^@/]*@#\1#')
case "$O" in https://*) ;; *) die "只支持 https 仓库地址：$O" ;; esac
REMOTE="https://$(printf '%s' "${CONN_USERNAME:-git}" | sed 's/@/%40/g'):${CONN_TOKEN}@${O#https://}"
git config user.name >/dev/null 2>&1 || git config user.name "Flow"
git config user.email >/dev/null 2>&1 || git config user.email "flow@localhost"
`
