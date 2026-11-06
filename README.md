# Flow

自托管的轻量 CI/CD 流水线（对标云效 Flow）：PostgreSQL（或内存）存储 + Shell / Docker / Kubernetes Runner；手动 / API / Git Webhook / cron 触发；加密密钥、Runner 标签、代码拉取、缓存与制品（本地或 S3/MinIO）、人工卡点、矩阵、重试、超时、钉钉/飞书/企业微信通知；用户登录、多租户与角色权限。

## 架构

```
┌─────────── Server (cmd/server) ───────────┐        ┌──── Runner (cmd/runner) ────┐
│ webui    内嵌前端（/）                      │        │                             │
│ api      HTTP API（用户 / Runner / Hook）   │◀─poll──│ 领取 Job（按标签）          │
│ auth     登录会话、API 令牌、租户与角色     │──job──▶│ 拉代码 → 解压上游产物 →     │
│ engine   Run 实例化、DAG 推进、租约、卡点、 │+secrets│ 恢复缓存 → 执行 Step        │
│          重试、密钥解密、产物、通知         │        │ （shell 或 docker run）     │
│ trigger  cron 解析、Webhook 验签与过滤      │◀─logs──│ 日志按密钥值脱敏后批量上传  │
│ store    持久化（PostgreSQL / 内存）        │◀─tar───│ 成功后保存缓存、上传产物    │
│ blob     产物存储（本地目录 / S3 兼容）     │        │                             │
└───────────────────────────────────────────┘        └─────────────────────────────┘
```

- **调度**：Job 状态 `pending → queued → running → success|failed|canceled`，上游失败的 Job 置为 `skipped`；`when` 可改为上游失败时或总是执行。人工卡点 Job 依赖满足后进入 `waiting`，通过视为成功、驳回视为失败。
- **租约**：Runner 领取 Job 时获得 30s 租约，靠心跳续期；租约过期（Runner 失联）由 Server 每 5s 回收为 `failed`（配置了 `retry` 时重新排队）。
- **重试**：`retry: N` 失败后自动重新排队（`attempt` 递增，日志追加）；已结束的失败 / 取消运行可「重试失败的 Job」，成功的 Job 保留结果。
- **步骤进度**：Runner 逐步上报步骤的开始 / 结束，运行详情显示每个步骤的状态、耗时与退出码，点击跳到对应日志。
- **并发控制**：`concurrency` 声明的并发组内同一时间只有一个运行在执行（PostgreSQL advisory lock 保证多实例下判定一致），其余排队；运行结束（含取消）时启动最早排队的运行；重试的运行同样排队。
- **提交状态**：`source.report_status` 开启后，运行开始执行且提交已知时写入「进行中」，结束时随通知写入结果。推送 / 合并请求的提交说明（标题）含 `[skip ci]`、`[ci skip]`、`[no ci]` 或 `[skip flow]` 时不触发运行。
- **审计**：成员的写操作（流水线配置、运行 / 取消 / 重试 / 回滚、审批、触发器、密钥、模板、环境与主机、成员、Runner、令牌）在成功后记录操作人、对象与来源 IP，「设置 → 审计日志」中按类型与操作人查询，保留一年。
- **回滚**：成功的运行可「回滚到此次运行」：新建一次运行，使用该运行当时的配置版本、运行参数与代码提交（当前配置不变）。每次运行记录检出的提交：Webhook 触发时即确定，否则取首个拉取代码的 Job 上报的提交，之后领取的 Job 检出同一提交。配置本身可在「配置历史」中回退到任一版本（生成新版本并立即生效）。
- **取消**：未开始的 Job 直接取消；运行中的 Job 通过心跳响应通知 Runner，Runner 杀掉整个进程组（或 `docker rm -f`）。
- **并发**：每次状态迁移是一个数据库事务，修改 Run 下的 Job 前先 `SELECT ... FOR UPDATE` 锁住该 Run 行；领取任务用 `FOR UPDATE SKIP LOCKED`，死锁 / 串行化冲突自动重试。Server 无进程内锁，可多实例部署。
- **触发器**：Webhook 按触发器各自的密钥验签（GitHub/Gitea/Gogs HMAC、GitLab/Gitee Token），解析推送与合并请求事件，按分支 / 标签通配过滤后创建 Run，按投递 ID 去重（与创建 Run 同一事务，唯一键保证并发重试只建一个 Run）；cron 由 Server 每 5s 扫描到期触发器（`SKIP LOCKED` 领取并在同一事务推进下次时间，多实例不会重复触发），停机期间错过的时刻只补一次。
- **密钥**：AES-256-GCM 加密存储（附加数据绑定租户 / 流水线 / 名称，密文不能挪用），值只写不读；Runner 领取 Job 时随任务下发明文，注入为同名环境变量（覆盖 YAML 中同名变量），Runner 在上传前把日志中出现的密钥值替换为 `***`（逐行匹配，4 字符以下的值不脱敏）。主密钥来自 `FLOW_SECRET_KEY` 或 `data/secret.key`，多实例须共享、务必备份。
- **Runner**：在界面注册，每个 Runner 持独立 Token（库中只存哈希），只领取本租户的 Job，Job 的 `runs-on` 须是 Runner 上报标签的子集；注册 Runner 的标识由 Server 决定，不能冒用他人租约。`FLOW_RUNNER_TOKEN` 是可选的共享 Token（可领取所有租户的 Job），适合单团队自用。
- **产物与缓存**：Job 成功后把 `artifacts` 匹配的文件打包为 tar.gz 流式上传到 Server（本地目录或 S3 兼容存储），所有下游（含间接下游）Job 执行前自动下载解压；重试时覆盖。`cache` 存在 Runner 本地，按租户 + 展开后的 key 区分，成功后保存。解压时拒绝逃逸工作目录的路径和符号链接。
- **通知**：运行结束时按流水线 `notify` 配置发送钉钉 / 飞书 / 企业微信机器人或通用 Webhook；在事务中锁住 Run 发送，多实例不会重复发送，失败只记日志不重试。
- **用户与权限**：用户名密码登录（PBKDF2），会话为 HttpOnly Cookie（Cookie 鉴权的写请求须带 `X-Flow-Request` 头防 CSRF），脚本使用个人访问令牌（`Authorization: Bearer`）。用户可属于多个租户，每个租户内角色为只读 / 开发者 / 管理员；所有资源按当前租户隔离，其他租户的资源一律返回 404。系统管理员可新建租户，在所有租户中视为管理员。
- **Step 环境**：只继承 PATH/HOME 等白名单变量，不会拿到 Runner Token。
- **流水线权限**：每条流水线有成员与角色（内置管理员 / 开发者 / 运行者 / 查看者，可在「角色管理」自定义），权限细分为查看、运行、编辑、删除、删除运行记录、管理成员；创建人与租户管理员拥有全部权限，公开流水线所有成员可查看，租户只读成员最多查看。
- **构建节点**：自建节点（`/api/runner/install.sh` 一条命令安装 flow-runner 并注册为 systemd 服务）、GitHub 节点（任务排队时调用 `workflow_dispatch` 触发仓库工作流，工作流中 `flow-runner -once` 领取一个任务后退出；10 分钟未被领取重新触发）、主机 Agent（主机以 Agent 方式接入时，部署 Runner 把上传内容与脚本经 Server 交给主机上的 `flow-runner -agent` 执行并转发日志，主机无需开放 SSH；Agent 失联 60s 后任务失败）。节点上报系统、架构、主机名、版本与并发；任务 `runs-on: [node:<名称>]` 只在该节点执行。
- **内置步骤与服务连接**：步骤写 `kind:` + `with:`（参数注入为 `INPUT_*`）使用内置步骤库（环境安装、构建、Docker、Git、通知、OSS / Cloudreve、构建物上传下载等，`GET /api/step-kinds` 列出）；`connection:` 引用「服务连接」（Git 平台、镜像仓库、npm、OSS、Cloudreve、Bark / 企业微信 / Telegram / 邮件），字段以 `CONN_*` 注入并脱敏。步骤间用 `$FLOW_ENV` / `$FLOW_PATH` / `$FLOW_OUTPUT` 传递变量、PATH 与任务输出，下游任务以环境变量读取上游输出，`if:` 表达式（`== != =~ !~ && || !`）决定是否执行；因条件不满足（或 `when: on_failure` 未触发）而跳过的任务视为已满足依赖，不阻塞下游，上游失败导致的跳过才向下传递。
- **变量**：流水线变量（可设为运行时输入）、通用变量组（租户级，私密变量加密并脱敏）、流水线级 / 租户级密钥，优先级依次升高；`cache_dirs` 为容器任务挂载按流水线隔离的缓存目录。
- **任务插件与审批消息**：任务结束时按状态发送插件消息（钉钉 / 飞书 / Webhook 或服务连接）；人工卡点进入待审批时经服务连接发送带签名「通过 / 驳回」链接的消息（需 `FLOW_PUBLIC_URL`）。

## 目录

| 路径 | 说明 |
|---|---|
| `cmd/server` | 控制面入口 |
| `cmd/runner` | 执行节点入口 |
| `internal/pipeline` | YAML 解析与校验（重名、依赖、环检测） |
| `internal/engine` | 调度核心；`trigger.go` 为触发器管理、Webhook 处理与 cron 调度；`templates.go` 与 `builtin/` 为模板管理与内置流水线模板 |
| `internal/trigger` | 无状态逻辑：cron 表达式（5 段、宏、时区、夏令时）、Webhook 验签、推送 / 合并请求事件解析、分支过滤 |
| `internal/store` | 存储接口、PostgreSQL 实现（`postgres.go`）、内存实现（`memory.go`）；`storetest` 让测试在两种存储上各跑一遍 |
| `internal/api` | HTTP 路由与鉴权；`e2e_test.go`、`features_test.go` 为端到端测试 |
| `internal/runner` | Runner：轮询、执行、日志上传；`workspace.go` 拉代码 / 产物 / 缓存，`archive.go` tar.gz 打包与安全解压，`kubernetes.go` 为 Kubernetes 执行器（控制进程与 Pod 内阶段） |
| `internal/k8s` | 最小 Kubernetes API 客户端（Pod / Secret，无 client-go 依赖）；`k8stest` 为测试用假 API 与进程内 kubelet |
| `internal/auth` | 用户、会话与 API 令牌、租户成员与角色 |
| `internal/secret` | 密钥加解密（AES-GCM）、日志脱敏 `Masker` |
| `internal/blob` | 产物存储：本地目录、S3 兼容（自带 SigV4 签名，无 SDK 依赖） |
| `internal/notify` | 钉钉 / 飞书 / 企业微信 / 通用 Webhook 通知 |
| `internal/gitstatus` | 提交状态回写（GitHub / GitLab / Gitea） |
| `internal/sshx` | 主机部署用的 SSH：连接（校验 / 记录主机公钥）、执行脚本、tar 流上传；`sshtest` 为测试用 SSH 服务端 |
| `internal/junit` | JUnit XML 测试报告解析与汇总 |
| `internal/steps` | 内置步骤库：`kind` → 脚本（`defs_*.go`），公共 shell 函数前导 |
| `internal/connect` | 服务连接的发送（Bark / 企业微信 / Telegram / SMTP）与连接测试（Git 平台、npm、OSS、Cloudreve） |
| `internal/version` | 构建版本（`-ldflags -X flow/internal/version.Version=…`），节点上报 |
| `scripts/dev-env.sh` | 开发环境：Go 与 PostgreSQL 的安装、独立集群启动与环境变量导出（容器重建后自动恢复） |
| `migrations/` | SQL 迁移（嵌入二进制，Server 启动时按文件名顺序自动执行；所有表含 `tenant_id`；已执行的文件不得修改，变更新增文件）：016 流水线成员 / 角色 / 公开、会话信息；017 通用变量组、服务连接、运行备注与并发上限；018 任务输出、通知标记、命名构建物；019 流水线分组；020 构建节点类型 / 环境信息、主机接入方式、Agent 主机任务 |
| `web/` | 前端：Vite + React + TypeScript，React Flow 画 DAG，xterm.js 显示日志；`src/editor/` 为可视化流水线编辑器（`model.ts` 在 YAML 文档树上增删改，保留注释与未改动的格式；`Canvas.tsx` 画布；`Panels.tsx` 属性面板；`presets.ts` 任务组件库；`History.tsx` 配置历史、差异与回退；`YamlEditor.tsx` + `yamlAssist.ts` 基于 CodeMirror 的 YAML 模式：问题定位到行、按层级补全字段；`StepTemplateForm.tsx` 步骤模板表单）；界面布局参照 Code Flow：顶栏「仪表盘 / 流水线 / API Key / 账户设置」，流水线列表与账户设置为左侧分区导航；流水线详情分「最近运行 / 运行历史 / 部署历史 / 统计报表」（部署历史 `components/Deliveries.tsx` ← `GET /api/pipelines/{id}/deliveries`，`engine/deliveries.go` 按运行汇总构建物、网盘上传、OSS 上传与主机部署；网盘 / OSS 记录取自步骤输出 `jobs.step_states[].outputs`，由 Runner 每步上报；构建物另有免登录签名下载 `/api/open/artifacts/{id}/download?sig=`，即输出 `ARTIFACT_<名称>_URL`；「构建物上传」可选对象存储服务连接，blob key 以 `ext/<连接ID>/<Bucket>/` 开头，读写删经 `engine/extblob.go` 路由，`expire_days` 覆盖保留天数），运行详情另有触发详情（`runs.trigger_info`：Webhook 提交列表 / 对比链接 / 覆盖变量）、排队原因（`GET /api/runs/{id}/queue`，`engine/queue.go`）与强制结束（`POST /api/runs/{id}/cancel?force=true`），编辑页（`#/pipelines/{id}/edit/{basic|triggers|vars}`）分「基本信息 / 流程配置 / 触发设置 / 变量和缓存」，流程配置中任务与阶段的属性在右侧抽屉中编辑（`PipelineEditor` 的 `section`，设置分区由 `Panels.tsx` 的 `PipelineSettings` 渲染）；`components/StageBoard.tsx` 为运行的阶段看板（点任务 / 步骤打开日志窗口），`components/Icon.tsx` 为线性图标；`src/pages/` 中 `Dashboard.tsx` 为仪表盘、`NewPipeline.tsx` 为新建向导、`Hosts.tsx` 为环境与主机、`PipelineStats.tsx` 为统计、`Audit.tsx` 为审计日志 |
| `internal/webui` | `go:embed` 前端构建产物（`web/` 构建输出到 `internal/webui/dist`） |
| `examples/` | 示例流水线 |
| `deploy/` | Server / Runner 镜像 Dockerfile（从源码构建），`kubernetes/runner.yaml` 为 Kubernetes 执行器部署清单；`docker/` 为 Docker Compose 部署（预编译二进制的运行镜像 + Server / 内置构建节点编排，见「Docker 部署」） |
| `scripts/package-docker.sh` | 打包 Docker 部署目录：构建前端、交叉编译 Server 与各平台 Runner，输出 `build/flow-docker(.tar.gz)` |

## 快速开始

需要 Go 1.23+、Node 18+、PostgreSQL 15+（不配置数据库时使用内存存储，重启即丢数据）。

开发环境可用 `scripts/dev-env.sh` 一键准备并在容器重建后恢复：缺 Go 时下载安装（安装包缓存在 `FLOW_DEV_HOME`，默认 `~/.claude/envcache/flow`），缺 PostgreSQL 时 apt 安装，并在 `FLOW_DEV_HOME/pgdata` 启动独立集群（角色 `flow/flow`，库 `flow_dev`、`flow_test`），数据随该目录保留；`eval "$(scripts/dev-env.sh --env)"` 导出 PATH 与数据库地址（`FLOW_DEV_DATABASE_URL`、`FLOW_TEST_DATABASE_URL`）。

```bash
# 构建前端（产物嵌入 Server 二进制；未构建时 / 会提示先构建）
(cd web && npm ci --include=dev && npm run build)

# 终端 1：Server（启动时自动迁移；首次启动创建管理员 admin，密码取 FLOW_ADMIN_PASSWORD，未设置则随机生成并打印到日志）
FLOW_DATABASE_URL=postgres://flow:flow@localhost:5432/flow FLOW_ADMIN_PASSWORD=change-me-now go run ./cmd/server

# 浏览器打开 http://localhost:8080 登录 → 「账户设置 → 构建节点」添加节点，复制只显示一次的 Token
#（或在节点机器上执行界面给出的 curl …/api/runner/install.sh | FLOW_RUNNER_TOKEN=… sh）
# 终端 2：Runner
FLOW_RUNNER_TOKEN=fr_xxx FLOW_RUNNER_LABELS=linux,docker go run ./cmd/runner -concurrency 2

# 终端 3（可选）：用 API 创建流水线并触发
TOKEN=$(curl -s localhost:8080/api/auth/login -d '{"username":"admin","password":"change-me-now"}' | jq -r .token)
H="Authorization: Bearer $TOKEN"
PID=$(curl -s -H "$H" --data-binary @examples/demo.yml localhost:8080/api/pipelines | jq -r .id)
RUN=$(curl -s -H "$H" -X POST localhost:8080/api/pipelines/$PID/runs -d '{"env":{"TARGET":"prod"}}' | jq -r .run.id)
curl -s -H "$H" localhost:8080/api/runs/$RUN | jq '.run.status, [.jobs[] | {name, status}]'
```

单团队自用也可以不注册 Runner：Server 和 Runner 都设置同一个 `FLOW_RUNNER_TOKEN`（共享 Token）。

界面（顶部导航：流水线 / 环境与主机 / 模板 / 设置）：
- **流水线**：列表支持搜索、分组、收藏、只看最近失败，显示最近 10 次运行趋势与成功率；「新建流水线」向导（模板库 → 名称 / 代码库 / 部署目标 → 画布确认）。
- **编排**：阶段 / 任务卡片画布（拖动调整顺序与所属阶段、从卡片右侧圆点连线添加依赖、Delete 删除）；任务分「执行命令 / 主机部署 / 人工卡点」三类；「+ 添加任务」从组件库选择构建、测试、主机部署（通用 / Java / Nginx / systemd / Docker 容器）、镜像与 Kubernetes、团队步骤模板；右侧面板编辑任务、步骤、部署目标、矩阵、产物、测试报告、缓存、运行参数、并发控制、代码源与提交状态回写、通知；撤销 / 重做、Ctrl+S 保存、实时校验；YAML 代码编辑器（高亮、折叠、搜索、错误定位到行、按层级补全）；配置历史、差异对比与一键回退，多人同时编辑时保存前提示冲突。
- **运行**：DAG 状态图、步骤进度（或部署任务的逐台主机进度）、实时日志、测试报告（失败用例与原因）、产物下载、卡点 / 部署审批、并发排队提示、取消 / 重试 / 回滚到此次运行；流水线「统计」页（成功率、耗时分布、恢复时间、最慢 / 不稳定任务、测试与部署频率）。
- **环境与主机**：环境概览（主机组健康状况、各流水线最近部署）、主机管理（SSH 凭据、连接测试、主机指纹）、部署记录。
- **模板**：流水线模板画布编辑、步骤模板表单编辑；每次保存生成版本，可查看历史、对比差异（含名称 / 描述变化）、载入或一键回退，多人同时编辑时提示冲突；画布中引用步骤模板时可选「最新」或锁定某个版本。
- **账户设置**：常规（个人资料）、构建节点、环境与主机、通用变量、服务连接、模板、团队成员、角色管理、审计日志、安全（改密码、登录会话）、危险操作（删除账户）；API Key（有效期）；右上角切换租户。

前端开发：先启动 Server（:8080），再 `cd web && npm run dev`，Vite 会把 `/api` 代理到 `FLOW_BACKEND`（默认 `http://localhost:8080`）。

> `--include=dev`：环境里设置了 `NODE_ENV=production` 时 npm 默认跳过 devDependencies（vite、typescript），会导致构建失败。

测试：`go test -race ./...`。设置 `FLOW_TEST_DATABASE_URL`（需建 schema 权限）后同时跑 PostgreSQL 用例，每个用例使用独立 schema，结束后自动删除：

```bash
FLOW_TEST_DATABASE_URL=postgres://flow:flow@localhost:5432/flow_test go test -race ./...
```

## Docker 部署

服务器上不需要 Go / Node：本地打包预编译二进制，服务器只构建一个 alpine 运行镜像（Server 与内置构建节点共用）。

```bash
scripts/package-docker.sh            # 输出 build/flow-docker.tar.gz（VERSION=… 指定版本号，SKIP_WEB=1 跳过前端构建）
# 上传到服务器并解压，例如 /opt/1panel/docker/compose/flow，然后：
cp .env.example .env                 # 填写数据库、端口、外部地址、admin 初始密码
docker build -t flow:latest .        # 或 docker compose build
docker compose up -d
```

- 首次启动后在「账户设置 → 构建节点 → 添加节点」创建自建节点，把令牌填到 `.env` 的 `FLOW_RUNNER_TOKEN`，再 `docker compose up -d`，内置构建节点（`flow-runner` 容器，标签 `linux,docker`）即上线。
- 内置构建节点挂载宿主 `/var/run/docker.sock`，指定了 `image` 的任务在宿主 Docker 中执行；工作目录 `FLOW_RUNNER_WORKDIR`（默认 `/opt/flow-runner`）在宿主与容器内路径相同，任务容器才能挂载到它。
- `./data` 保存主密钥 `secret.key` 与本地产物，需随数据库一起备份。
- **1Panel**：解压到 `/opt/1panel/docker/compose/flow` 后会出现在「容器 → 编排」中，可在面板里查看日志、重启、编辑 compose。使用面板安装的 PostgreSQL 时，在其容器里建库（`CREATE ROLE flow LOGIN PASSWORD '…'; CREATE DATABASE flow OWNER flow;`），`.env` 中主机名写 `postgresql`，并设置 `FLOW_NETWORK=1panel-network`、`FLOW_NETWORK_EXTERNAL=true`；再在「数据库 → PostgreSQL」中「从服务器获取」即可在面板中管理与备份。
- 升级：重新打包上传，覆盖 `bin/`、`dist/` 后 `docker build -t flow:latest . && docker compose up -d`（迁移在 Server 启动时自动执行）。
- Docker 24 自带的 BuildKit 在部分网络环境下解析基础镜像元数据会卡住，可用 `DOCKER_BUILDKIT=0 docker build -t flow:latest .`。

## 配置

| 变量 | 组件 | 默认值 / 说明 |
|---|---|---|
| `FLOW_ADDR` | server | `:8080` |
| `FLOW_DATABASE_URL` | server | 空 = 内存存储 |
| `FLOW_DATA_DIR` | server | `data`：存放主密钥文件 `secret.key` 和本地产物 `artifacts/` |
| `FLOW_SECRET_KEY` | server | 32 字节主密钥（base64 或 hex，`openssl rand -base64 32`）；设置后不使用密钥文件，多实例部署必须设置 |
| `FLOW_ADMIN_PASSWORD` | server | 首次启动（无任何用户）时 admin 的密码；空则随机生成并打印 |
| `FLOW_PUBLIC_URL` | server | 界面外部地址，用于通知中的详情链接；`https://` 开头时会话 Cookie 加 `Secure` |
| `FLOW_S3_BUCKET` | server | 设置后产物存入 S3 兼容存储，同时需要 `FLOW_S3_ENDPOINT`、`FLOW_S3_ACCESS_KEY`、`FLOW_S3_SECRET_KEY`；可选 `FLOW_S3_REGION`（默认 us-east-1）、`FLOW_S3_PREFIX`、`FLOW_S3_PATH_STYLE`（默认 true，MinIO 适用；OSS / COS 设为 false） |
| `FLOW_ARTIFACT_RETENTION` | server | `30d`：产物默认保留时长（如 `7d`、`72h`），`0` 为永久；流水线 `retention.artifacts` 可覆盖 |
| `FLOW_RUNNER_TOKEN` | 两者 | Server：可选的共享 Token；Runner：注册 Token（`fr_` 开头）或共享 Token |
| `FLOW_SERVER` | runner | `http://localhost:8080` |
| `FLOW_RUNNER_LABELS` | runner | 逗号分隔的标签，用于匹配 `runs-on` |
| `FLOW_RUNNER_ID` | runner | 主机名；共享 Token 时作为 Runner 标识；Kubernetes 执行器用它识别并清理本 Runner 遗留的 Pod，应设为固定值 |
| `FLOW_WORKDIR` | runner | `$TMPDIR/flow-runner` |
| `FLOW_CACHE_DIR` | runner | `$FLOW_WORKDIR/cache` |
| `FLOW_CONCURRENCY` | runner | `1` |
| `FLOW_EXECUTOR` | runner | `shell`（主机 / Docker）或 `kubernetes`（每个 Job 一个 Pod，见下文） |
| `FLOW_RUNNER_DIST` | server | 存放 `flow-runner-<os>-<arch>` 的目录，供安装脚本与 GitHub 节点下载；未设置时查找 Server 同目录下同平台的 `flow-runner`（server 镜像内置 `/dist`） |
| `FLOW_MODE=agent` / `-agent` | runner | 作为主机 Agent 运行（只执行部署到本机的任务） |
| `FLOW_RUNNER_ONCE=1` / `-once -wait 5m` | runner | 领取并执行一个任务后退出，等待超时未领到也退出（GitHub 节点） |

Runner 主机需要 `git`（配置了 `source` 时）和 `docker`（Job 指定 `image` 时）。代码拉取、产物与缓存解压都在主机工作目录中进行，容器内挂载为 `/workspace`。

## 流水线 YAML

```yaml
name: demo              # 租户内唯一
env: {KEY: value}       # 优先级：流水线 < Job < 触发时传入 < 内置变量；同名密钥覆盖以上全部
params:                 # 可选：运行参数，手动 / API 运行时填写（界面弹出表单），注入为同名变量
  - name: TARGET
    type: choice        # string（默认）/ choice / boolean
    options: [staging, prod]
    default: staging    # 未填时取默认值；自动触发（Webhook / 定时）也会补默认值；boolean 缺省为 false
    description: 部署环境
  - name: VERSION
    required: true      # 手动 / API 运行时必须有值，否则拒绝触发
source:                 # 可选：Runner 在执行 Step 前浅克隆代码到工作目录
  url: https://git.example.com/team/app.git
  branch: main          # 默认分支；Webhook 触发时用推送的分支 / 标签与提交，手动运行可传 FLOW_GIT_BRANCH 覆盖
  token_secret: GIT_TOKEN   # 可选：访问令牌所在的密钥名（HTTP Basic，用户名默认 oauth2，可用 username 修改）
  depth: 50             # 默认 50
  report_status: true   # 可选：把运行结果回写为提交状态（GitHub / GitLab / Gitea 的提交与合并请求页显示），令牌需有写状态权限
  provider: gitlab      # 可选：github / gitlab / gitea，省略时按仓库地址推断（github.com、主机名含 gitlab / gitea）
notify:                 # 可选：运行结束通知
  - type: dingtalk      # dingtalk / feishu / wecom / webhook
    url: https://oapi.dingtalk.com/robot/send?access_token=${DING_TOKEN}   # ${NAME} 替换为同名密钥
    secret: ${DING_SECRET}   # 钉钉 / 飞书加签密钥，可选
    on: [failed]        # success / failed / canceled，默认 [failed]
retention:              # 可选：保留策略，Server 每小时清理一次
  artifacts: 7d         # 产物保留时长（7d / 36h），"0" 为永久；默认取 FLOW_ARTIFACT_RETENTION
  runs: 200             # 最多保留的运行数，更早的已结束运行连同日志、产物删除；默认不限
concurrency:            # 可选：同一并发组同时只执行一个运行，其余以 queued 状态排队，前一个结束后按顺序开始
  group: deploy-${TARGET}   # 省略为只与本流水线互斥；租户内同名的组跨流水线互斥；可用运行参数和 env
  cancel_previous: true # 可选：新运行取消同组中未结束的运行（适合只关心最新提交的预览 / 测试环境）
stages:                 # Stage 之间默认串行
  - name: build
    jobs:               # Stage 内并行
      - name: compile   # 全局唯一
        image: golang:1.23   # 可选，设置后在容器中执行，工作目录挂载为 /workspace
        runs-on: [linux, docker]  # 可选：只由带有全部这些标签的 Runner 领取
        needs: [other]       # 可选：只依赖所列 Job；needs: [] 表示无依赖；不写则依赖上一 Stage 全部 Job
        when: on_success     # on_success（默认）/ on_failure（本次运行已有 Job 失败）/ always
        matrix: {GOOS: [linux, darwin], GOARCH: [amd64, arm64]}  # 展开为 4 个 Job，取值注入为同名变量，最多 64 个
        timeout: 30m         # 超时视为失败
        retry: 2             # 失败后自动重试次数，最多 5
        checkout: false      # 配置了 source 但本 Job 不需要代码时关闭
        cache: {key: "go-$GOOS", paths: [.gocache]}   # Runner 本地缓存，key 可引用变量
        artifacts: [dist, "*.tar.gz"]                  # 成功后上传，下游 Job 自动获得；支持 * ? []，目录递归
        env: {GOFLAGS: -mod=mod}
        reports: {junit: ["reports/*.xml"]}  # 可选：步骤结束后（失败也会）解析 JUnit XML，运行详情展示用例统计与失败原因
        steps:          # 顺序执行，sh -e，同一 Job 内共享工作目录
          - name: build
            run: go build -o dist/ ./...
            env: {TAG: "v${VERSION}"}   # 可选：仅本步骤可见，${NAME} 在执行时按 Job 变量与密钥展开
          - name: image
            uses: docker-build         # 引用租户的步骤模板（见下文「模板」），创建运行时展开；docker-build@3 锁定第 3 版
            with: {IMAGE: registry.example.com/app}
  - name: gate
    jobs:
      - name: release-approval
        type: approval       # 人工卡点：不需要 Runner，依赖满足后等待审批
        approvers: [alice]   # 可选：审批人用户名，空为任意开发者及以上成员（管理员总是可以审批）
  - name: deploy
    jobs:
      - name: deploy-prod
        deploy:              # 主机部署：步骤通过 SSH 在主机组的每台主机上执行（主机组在「环境与主机」中维护）
          hosts: prod-web    # 主机组名；保存时校验存在
          upload: dist       # 可选：上传工作目录中的目录（上传其内容）或文件，通常是上游产物
          target: /opt/apps/app   # 远程目录，默认 /tmp/flow-deploy/<流水线名>；上传到这里并作为脚本工作目录
          batch: 1           # 每批主机数，默认 1（逐台滚动）；前一批全部成功才继续，失败即停止
        steps:
          - run: ./restart.sh   # 远程以 set -e 执行；可用 $FLOW_DEPLOY_HOST、$FLOW_DEPLOY_DIR 与流水线变量、密钥
```

**主机部署**：在「环境与主机」中添加服务器（地址、端口、用户、SSH 私钥或密码，凭据加密保存），加入主机组，主机组归属于环境（开发 / 测试 / 预发 / 生产…）。连接测试从 Server 发起，首次成功时记录主机公钥，之后部署时校验（不一致即拒绝，主机重装后可「重置指纹」）。部署由 Runner 执行：领取任务时随任务下发目标主机与解密后的凭据，Runner 准备工作区（上游产物、可选代码），打包 `upload` 后按批次 SSH 到每台主机，`tar` 解压到 `target` 并依次执行步骤，日志按主机加前缀，进度逐台上报；部署任务默认不拉取代码（`checkout: true` 开启），不能设置 `image`。环境可设置「部署前需要审批」（可指定审批人）：部署任务就绪后先进入待审批，通过后才执行；重试失败的运行需重新审批。待审批的卡点与部署汇总在「待我审批」（顶栏显示数量）中。每次部署记录在「部署记录」中，环境页展示各流水线在该环境的最近部署。

- **跳板机**：主机可指定经由另一台主机（堡垒机）连接，等同 `ssh -J`，只支持一级；连接测试与部署都经跳板机转发，跳板机的主机公钥同样首次记录后校验。被引用的跳板机不能删除。
- **环境变量与密钥**：环境可配置变量和密钥（密钥加密保存、只写不读），在部署到该环境的任务执行时注入：环境变量覆盖流水线中的同名变量，环境密钥覆盖租户 / 流水线的同名密钥，日志中脱敏。同一条流水线部署到不同环境时用各自的配置。
- **按环境重新部署**：部署记录或运行详情中的「重新部署此版本」新建一次运行，只执行该部署任务（其余任务跳过），使用原运行的配置版本、参数、代码提交与**构建产物**，不重新构建、不影响其他环境；环境审批仍然生效。原运行的产物已过期时拒绝。

- 内置变量：`FLOW_PIPELINE`、`FLOW_RUN_ID`、`FLOW_TRIGGER`（`manual` / `api` / `webhook` / `cron`）、`FLOW_STAGE`、`FLOW_JOB`。
- `needs` 可引用矩阵 Job 的原名，表示依赖其全部展开项；展开后的 Job 名形如 `compile (linux, amd64)`。
- 保留策略：产物过期时间在上传时按创建运行时的配置计算（之后修改 `retention` 只影响新运行）；**每条流水线最近一次成功运行的产物始终保留**，便于发布 / 回滚。产物过期后重试依赖它的 Job 将拿不到上游产物。
- 完整示例见 `examples/full.yml`。

## 模板

在「模板」页管理，开发者及以上可编辑：流水线模板用画布编排，步骤模板用表单编辑输入参数和步骤（都可切换到 YAML）。「新建流水线」是一个向导：从模板库（按语言 / 技术栈分类，卡片带流程缩略图）选择起点 → 填写名称、分组、代码仓库、部署目标主机组、是否推送即运行 → 在画布中确认后创建。内置模板包含 Java / Go / 前端 / 容器服务的「构建并部署到主机」、Kubernetes 滚动发布、Python 测试打包等。

**步骤模板**：可复用的一组步骤，名称为小写字母、数字、`.` `_` `-`。流水线中 `uses: 模板名` 引用，`with` 传入的输入注入为模板各步骤的环境变量：

```yaml
description: 构建并推送镜像
inputs:
  IMAGE: {required: true, description: 镜像名}
  DOCKERFILE: {default: Dockerfile}
steps:
  - name: build
    run: docker build -f "$DOCKERFILE" -t "$IMAGE:$FLOW_RUN_ID" .
  - name: push
    run: docker push "$IMAGE:$FLOW_RUN_ID"
```

- 展开后的步骤名为「引用步骤名 / 模板步骤名」；变量优先级：输入默认值 < `with` < 模板步骤 `env` < 引用步骤 `env`，密钥仍然最优先。模板中的步骤不能再 `uses` 其他模板。
- 保存流水线时校验引用（模板存在、必填输入齐全、无多余输入）。**创建运行时展开并随 Job 保存**：之后修改模板只影响新运行，已有运行（含重试）保持原样。
- 被流水线引用的模板不能改名或删除；修改会破坏引用最新版本的流水线（如新增必填输入）时拒绝保存，并指出受影响的流水线。
- **版本**：模板每次保存生成新版本（v1、v2…，内容无变化不生成），历史保留，可对比与回退（回退 = 以旧版本内容生成新版本）。`uses: name` 跟随最新版本；`uses: name@N` 锁定第 N 版，之后的修改不影响它，适合生产流水线先锁定、验证后再升级。删除模板会一并删除其历史。

**流水线模板**：新建流水线时的起点。内置 Go 应用、Node.js 前端、Docker 镜像、Java Maven、审批发布五个（只读），团队可在流水线详情页「另存为模板」或在模板页新建。

## 触发器

在流水线详情页「触发器」中添加，或调用 API。触发器可配置注入变量 `env`（优先级同“触发时传入”）。

**Git Webhook**：保存后生成回调地址 `/api/hooks/{id}` 和密钥，填到 Git 平台：

| 平台 | 配置 |
|---|---|
| GitHub / Gitea / Gogs | Content type 选 `application/json`，Secret 填密钥（校验 `X-Hub-Signature-256` 等 HMAC 签名）；事件勾选 Push / Pull requests |
| GitLab | Secret token 填密钥（`X-Gitlab-Token`）；勾选 Push / Tag push / Merge request events |
| Gitee | WebHook 密码或签名密钥填密钥（`X-Gitee-Token`）；勾选 Push / Tag Push / Pull Request |
| 自定义脚本 | 请求头 `X-Hub-Signature-256: sha256=<hex(HMAC-SHA256(密钥, 请求体))>`，请求体至少含 `ref`、`after`；可带 `X-Flow-Delivery` 去重 |

- 事件：`events` 可选 `push`（分支 / 标签推送，默认）与 `pull_request`（GitHub / Gitea / Gogs / Gitee 的 Pull Request、GitLab 的 Merge Request）。未订阅的事件、删除分支、未匹配过滤规则时返回 200 和 `skipped` 原因，创建 Run 时返回 202。
- 过滤：`branches` / `tags` 为通配符列表（`*` 不跨 `/`，`**` 跨层级）。推送事件：两者都为空时匹配所有分支、不匹配标签。合并请求按**目标分支**匹配 `branches`（为空匹配所有）。
- 路径过滤（仅推送事件）：`paths` 非空时推送改动的文件须匹配其一，`paths_ignore` 非空时改动的文件全部被忽略则不触发；以 `/` 结尾表示整个目录，`**/` 也匹配根目录。改动文件取自载荷中各提交的新增 / 修改 / 删除列表，一次推送超过 20 个提交（载荷可能截断）时不过滤。
- 提交说明（合并请求为标题）含 `[skip ci]`、`[ci skip]`、`[no ci]` 或 `[skip flow]` 时不触发。
- 合并请求只在新建、重新打开、源分支有新提交时触发（关闭、合并、改标题等不触发）。Runner 检出目标仓库中的 `refs/pull/N/head`（GitLab 为 `refs/merge-requests/N/head`），fork 的提交也能拉到。
- **fork 仓库的合并请求默认不运行**：fork 中的代码会拿到流水线可用的全部密钥。仅在信任所有贡献者时开启 `allow_forks`。
- 注入变量：`FLOW_GIT_EVENT`（`push` / `pull_request`）、`FLOW_GIT_REF`、`FLOW_GIT_COMMIT`、`FLOW_GIT_REPO`、`FLOW_GIT_URL`、`FLOW_GIT_PUSHER`、`FLOW_GIT_MESSAGE`；推送另有 `FLOW_GIT_BRANCH` 或 `FLOW_GIT_TAG`；合并请求另有 `FLOW_GIT_PR_NUMBER`、`FLOW_GIT_PR_ACTION`、`FLOW_GIT_PR_URL`、`FLOW_GIT_PR_SOURCE_BRANCH`、`FLOW_GIT_PR_TARGET_BRANCH`，且**不设置** `FLOW_GIT_BRANCH`（避免按分支判断的部署脚本被合并请求触发）。流水线配置了 `source` 时 Runner 自动检出对应提交，否则可在 Step 中自行 `git clone`。
- 投递去重：按平台的投递 ID（GitHub `X-GitHub-Delivery`、GitLab `Idempotency-Key` / `X-Gitlab-Event-UUID`、Gitea / Gogs `X-*-Delivery`）去重，平台超时重试或手动重发同一投递时返回 200、`"duplicate": true` 和原来的 Run。记录保留 7 天。Gitee 不提供投递 ID，不去重。

**cron**：标准 5 段（分 时 日 月 周），支持 `,` `-` `/`、`JAN`/`MON` 等缩写和 `@hourly` / `@daily` / `@weekly` / `@monthly` 宏；日与周同时限定时满足其一即可。`timezone` 为 IANA 时区名（时区数据已嵌入二进制），留空用 Server 本地时区。精度约 5 秒。

## API

除 `/api/auth/login`、`/api/hooks/*`、`/api/runner/*` 外均需登录：会话 Cookie（写请求带 `X-Flow-Request: 1` 头）或 `Authorization: Bearer <令牌>`（登录返回的 `token`，或「个人设置」中创建的 API 令牌）。所需角色：查看类为只读，流水线 / 运行 / 触发器 / 流水线密钥 / 审批为开发者，成员 / Runner / 租户密钥为管理员。

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/auth/login` · `/api/auth/logout` | 登录（`{"username","password"}`，写 Cookie 并返回 `token`）/ 登出 |
| GET | `/api/me` | 当前用户、租户、角色、可切换租户 |
| POST | `/api/me/tenant` · `/api/me/password` | 切换租户 `{"id"}` / 改密码 `{"old_password","password"}` |
| GET/POST/DELETE | `/api/me/tokens[/{id}]` | 个人访问令牌（创建时返回一次明文） |
| POST | `/api/tenants` | 新建租户 `{"id","name"}`（系统管理员） |
| GET/POST | `/api/members` | 成员列表 / 添加 `{"username","password","role"}`（用户不存在时需 password 创建） |
| PUT/DELETE | `/api/members/{user_id}` | 改角色 `{"role"}` / 移出租户；`POST .../password` 重置密码 |
| POST | `/api/pipelines` | 请求体为 YAML 原文 |
| GET | `/api/pipelines/{id}/stats?days=N` | 统计（默认 30 天，最多 180）：成功率、耗时 P50 / P90、平均恢复时间、每日运行、最近运行耗时、各任务失败率与耗时、测试与部署汇总 |
| GET/POST, PUT/DELETE | `/api/environments[/{id}]` | 部署环境 `{"name","description","rank","require_approval","approvers","variables","secrets","remove_secrets"}`（写操作需管理员；密钥只返回 `secret_names`） |
| GET/POST, PUT/DELETE | `/api/host-groups[/{id}]` | 主机组 `{"name","description","environment_id"}`；`GET .../users` 为部署到它的流水线；被引用时不能改名或删除 |
| GET/POST, PUT/DELETE | `/api/hosts[/{id}]` | 主机 `{"name","address","port","username","auth_type":"key"|"password","credential","group_ids","jump_host_id"}`；凭据只写不读，更新时为空保持不变 |
| POST | `/api/hosts/{id}/test` · `/api/hosts/{id}/reset-key` | 连接测试（首次成功记录主机公钥，开发者）/ 清除已信任的主机公钥（管理员） |
| GET | `/api/deployments?environment=&pipeline=&limit=` | 部署记录（倒序） |
| POST | `/api/runs/{id}/redeploy` | 只重新执行该运行中的部署任务 `{"job":"deploy-prod"}`，沿用当时的配置、参数与产物 |
| GET | `/api/approvals` | 当前用户可审批的等待中任务（人工卡点与部署审批） |
| GET | `/api/runs/{id}/tests` | 运行中各任务的测试报告（汇总 + 失败 / 跳过 / 最慢用例） |
| GET | `/api/audit?category=&actor=&target=&before=&limit=` | 审计日志（管理员）：成功的写操作，保留一年 |
| POST | `/api/pipelines/validate` | 按保存规则校验 YAML（含步骤模板引用），不保存；合法返回 204 |
| GET | `/api/pipelines` | 列表 |
| GET | `/api/pipelines/summary?runs=N` | 列表页数据：流水线 + 分组 + 当前用户是否收藏 + 各自最近 N 次运行（默认 10，最多 20） |
| PUT | `/api/pipelines/{id}/group` | 设置列表分组 `{"group":"后端"}`（空为移出分组），不产生新版本 |
| PUT/DELETE | `/api/pipelines/{id}/favorite` | 收藏 / 取消收藏（个人设置，只读成员也可用） |
| PUT | `/api/pipelines/{id}/info` | 说明与归档标记 `{"description","archived"}`，不产生新版本 |
| GET/PUT/DELETE | `/api/pipelines/{id}` | 查看 / 更新 YAML / 移到回收站；每次保存版本号加 1，PUT 可带 `?base_version=N`，期间被他人保存过时返回 409 而不覆盖 |
| GET | `/api/pipelines/trash` | 回收站：流水线不在列表中、不能保存或运行（触发器随之不触发），运行记录与产物保留 |
| POST | `/api/pipelines/{id}/recover` | 从回收站恢复 |
| DELETE | `/api/pipelines/{id}?purge=true` | 彻底删除回收站中的流水线（级联删除运行、日志、产物、触发器、流水线密钥） |
| GET | `/api/dashboard?days=7\|30\|90` | 仪表盘：流水线数（含我创建的）、本周期与上周期的运行数 / 成功率 / 平均耗时、构建节点在线数、每日趋势、进行中与最近的运行、流水线排行 |
| GET | `/api/pipelines/{id}/revisions[/{version}]` | 配置历史：最近 100 个版本（不含 YAML）/ 某个版本的完整 YAML；回退生成的版本带 `restored_from` |
| POST | `/api/pipelines/{id}/revisions/{version}/restore?base_version=N` | 回退到该版本：以其定义生成新版本并立即生效；内容与当前相同时不产生新版本 |
| POST | `/api/pipelines/{id}/runs` | 触发，可选 `{"env":{}}` |
| GET | `/api/pipelines/{id}/runs?limit=N&status=&trigger=&before=` | 运行历史（倒序，默认 50，最多 200）；可按状态、触发方式筛选，`before` 为编号用于翻页 |
| GET | `/api/runs/{id}` | Run 与全部 Job |
| GET | `/api/runs/{id}/events` | SSE：Run 详情变化时推送 `event: run`（数据同上） |
| POST | `/api/runs/{id}/cancel` · `/api/runs/{id}/retry` | 取消 / 重试失败的 Job |
| POST | `/api/runs/{id}/rollback` | 回滚到该次成功运行：按其配置版本、参数与代码提交新建运行（`trigger: rollback`，`rollback_of` 为原运行） |
| GET | `/api/runs/{id}/artifacts` · `/api/artifacts/{id}/download` | 产物列表 / 下载 tar.gz |
| GET | `/api/jobs/{id}/logs?offset=N` | 增量日志，响应头 `X-Next-Offset` |
| GET | `/api/jobs/{id}/logs/stream?offset=N` | SSE：`event: log`（`id` 为下一字节偏移，`data` 为 base64 日志块）、`event: end`（`{"status"}`）；断线带 `Last-Event-ID` 续传 |
| POST | `/api/jobs/{id}/approve` | 卡点审批 `{"approve":true,"comment":""}` |
| GET/POST | `/api/templates[?kind=steps\|pipeline]` | 模板列表（含内置流水线模板）/ 创建 `{"kind","name","description","yaml"}` |
| GET/PUT/DELETE | `/api/templates/{id}` | 查看 / 修改 `{"name","description","yaml"}`（`?base_version=N` 时期间被他人保存则 409）/ 删除；`GET .../users` 列出引用该步骤模板的流水线及所用版本（`versions`，0 为最新） |
| GET | `/api/templates/{id}/revisions[/{version}]` | 模板版本列表（倒序，最多 100，不含 YAML）/ 某版本内容 |
| POST | `/api/templates/{id}/revisions/{version}/restore[?base_version=N]` | 回退：以该版本内容生成新版本 |
| GET/PUT/DELETE | `/api/secrets[/{name}]` | 租户级密钥（PUT `{"value"}`，值不可读回） |
| GET/PUT/DELETE | `/api/pipelines/{id}/secrets[/{name}]` | 流水线级密钥 |
| GET/POST/DELETE | `/api/runners[/{id}]` | 构建节点列表 / 添加 `{"name","kind":"self"|"github","repo","workflow","ref","token"}`（返回一次节点 Token）/ 删除 |
| PUT | `/api/runners/{id}` | 修改节点（GitHub 访问令牌为空保持不变）；`POST .../token` 重置节点 Token；`GET .../workflow` GitHub 节点的工作流文件 |
| POST | `/api/hosts/{id}/agent-token` | 为 Agent 接入的主机生成（重置）Agent Token |
| GET | `/api/runner/install.sh` · `/api/runner/download/{os}/{arch}` | 安装脚本（`… \| FLOW_RUNNER_TOKEN=… sh [-s -- agent]`）/ flow-runner 二进制（无需登录） |
| GET/PUT/DELETE | `/api/pipelines/{id}/members[/{user}]` · `PUT /api/pipelines/{id}/public` | 流水线成员与角色 `{"role_id"}` / 公开 `{"public":true}` |
| GET/POST/PUT/DELETE | `/api/pipeline-roles[/{id}]` | 流水线角色 `{"name","permissions":["view","run","edit","delete","deleteRun","manageMembers"]}` |
| GET/POST/PUT/DELETE | `/api/var-groups[/{id}]` | 通用变量组 `{"name","description","vars":[{"name","value","secret"}]}`（私密值不回显） |
| GET | `/api/connection-types` · `/api/step-kinds` | 服务连接类型与字段 / 内置步骤目录 |
| GET/POST/PUT/DELETE | `/api/connections[/{id}]` · `POST /api/connections/test` | 服务连接 `{"type","name","values","visibility","allowed_users"}` / 测试 |
| POST | `/api/open/flow/webhook/{id}?token=` | 流水线 Webhook（通用触发，请求体 JSON 的 `env` 作为运行参数） |
| GET/POST | `/api/open/approve/{job}?action=approve\|reject&sig=` | 审批消息中的签名链接（GET 为确认页） |
| PUT | `/api/runs/{id}/remark` · `DELETE /api/runs/{id}` · `DELETE /api/pipelines/{id}/runs` | 运行备注 / 删除运行 / 删除全部已结束运行 |
| POST | `/api/pipelines/{id}/copy` · `/api/pipeline-groups[/{name}]` | 复制流水线 / 分组新建、改名、删除 |
| PUT/GET/DELETE | `/api/me/profile` · `/api/me/sessions[/{id}]` · `DELETE /api/me` | 个人资料 / 登录会话（设备、IP）与下线 / 删除账户 |
| GET/POST | `/api/pipelines/{id}/triggers` | 触发器列表 / 创建：`{"kind":"webhook","events":["push","pull_request"],"branches":["main"],"tags":["v*"],"allow_forks":false,"env":{}}` 或 `{"kind":"cron","schedule":"0 2 * * *","timezone":"Asia/Shanghai"}`，`enabled` 缺省 `true` |
| GET/PUT/DELETE | `/api/triggers/{id}` | 查看 / 修改（类型不可改）/ 删除 |
| POST | `/api/triggers/{id}/rotate-secret` | 重新生成 Webhook 密钥 |
| POST | `/api/hooks/{id}` | Git 平台回调（以触发器密钥鉴权） |
| POST | `/api/runner/claim` · `/api/runner/jobs/{id}/{heartbeat,logs,steps,hosts,tests,commit,complete,artifacts,host-tasks}` | Runner 协议（Bearer：注册 Token 或共享 Token；领取时上报节点环境）；`GET /api/runner/artifacts/{id}` 下载上游产物；`GET /api/runner/host-tasks/{id}?offset=&cancel=1` 部署 Runner 轮询 Agent 任务 |
| POST/GET | `/api/runner/agent/{claim,tasks/{id}/log,tasks/{id}/finish,tasks/{id}/upload}` | 主机 Agent 协议（仅 Agent Token）：领取、回传日志并续租（响应 `cancel`）、结束、下载上传内容 |

## Kubernetes 执行器

Runner 以 `FLOW_EXECUTOR=kubernetes` 运行在集群内时，为每个 Job 创建一个 Pod，而不是在本机执行：

```
flow-runner（控制进程，Deployment）── 领取 Job、心跳续租、取消 / 超时、上报结果
   │ 创建
   ▼
Job Pod ── init 容器 prepare（helper 镜像）：把 flow-runner 复制到共享卷，拉代码、解压上游产物、恢复缓存
        └─ 主容器 job（Job 的 image，缺省 FLOW_K8S_DEFAULT_IMAGE）：执行 Step、保存缓存、上传产物
```

- 部署：`deploy/runner.Dockerfile` 构建镜像（静态链接的 `/flow-runner` + git，同时用作控制进程与 helper 镜像），按 `deploy/kubernetes/runner.yaml` 注释修改后 `kubectl apply`。控制进程只需本命名空间内 Pod 的 create/get/list/delete 与 Secret 的 create 权限。
- 密钥与 Runner Token 放在每个 Job 一个的 Secret 中（以 ownerReference 随 Pod 回收），不出现在 Pod 定义里；Job Pod **不挂载** ServiceAccount Token。日志由 Pod 直接上报并按密钥值脱敏。
- 取消、超时时删除 Pod；镜像拉取失败（`ImagePullBackOff` 等）立即判定失败；调度等待原因写入日志。Job 失败时的退出码与原因经容器终止消息回传。控制进程重启时删除本 Runner（按 `FLOW_RUNNER_ID`）遗留的 Pod；控制进程失联导致租约过期后，Pod 上报日志会被拒绝并自行退出。
- 缓存：配置 `FLOW_K8S_CACHE_PVC`（需 ReadWriteMany）后挂载为缓存目录，否则 Job 的 `cache` 被忽略。
- 主容器镜像需要 `sh`；拉代码在 helper 镜像中完成，Job 镜像无需 git。

| 变量 | 说明 |
|---|---|
| `FLOW_K8S_HELPER_IMAGE` | 必填：含 `/flow-runner` 与 git 的镜像 |
| `FLOW_K8S_DEFAULT_IMAGE` | Job 未指定 `image` 时的镜像，默认 `alpine:3.20` |
| `FLOW_K8S_NAMESPACE` | Job Pod 的命名空间，默认控制进程所在命名空间 |
| `FLOW_K8S_SERVER_URL` | Pod 内访问 Server 的地址，默认同 `FLOW_SERVER` |
| `FLOW_K8S_SERVICE_ACCOUNT` / `FLOW_K8S_IMAGE_PULL_SECRETS` / `FLOW_K8S_PULL_POLICY` | Job Pod 的 ServiceAccount、拉取凭据（逗号分隔）、拉取策略 |
| `FLOW_K8S_NODE_SELECTOR` / `FLOW_K8S_REQUESTS` / `FLOW_K8S_LIMITS` | 如 `pool=ci`、`cpu=500m,memory=512Mi` |
| `FLOW_K8S_CACHE_PVC` | 缓存卷 PVC 名 |
| `FLOW_K8S_API` / `FLOW_K8S_TOKEN` / `FLOW_K8S_CA` | 在集群外运行控制进程时的 API 地址、Token、CA 证书 |

## 路线图

1. ~~**持久化**：PostgreSQL Store、自动迁移、多实例安全调度~~（已完成）
2. ~~**前端**：React Flow DAG、xterm.js 实时日志、流水线编辑与触发~~（已完成；日志与运行状态经 SSE 推送）
3. ~~**触发器**：Git Webhook（校验签名、过滤分支）、cron 定时触发~~（已完成；后续：路径过滤）
4. ~~**密钥**：加密存储，下发给 Runner 时注入，日志中按值脱敏~~（已完成；后续：主密钥轮换）
5. ~~**Runner 增强**：每个 Runner 独立 Token、标签匹配（`runs-on`）、缓存与制品上传（MinIO/S3）~~（已完成；缓存目前在 Runner 本地，后续可放到对象存储供多台 Runner 共享）
6. ~~**流程能力**：人工卡点、矩阵构建、失败重试、通知（钉钉/飞书）~~（已完成；另有 `when` 条件、超时、代码源自动拉取）
7. ~~**用户体系**：登录、RBAC，`tenant_id` 由认证上下文注入~~（已完成；后续：LDAP / OAuth 登录、审计日志、登录限流）
8. ~~**SSE 推送**：日志与 Run 状态实时推送，断线按字节偏移续传~~（已完成；进程内变更即时推送，多实例部署时其他实例的变更靠 2 秒兜底轮询，后续可用 PostgreSQL LISTEN/NOTIFY）
9. ~~**合并请求触发与投递去重**：GitHub / GitLab / Gitea / Gitee 的 PR / MR 事件，fork 默认不运行；按投递 ID 去重~~（已完成；后续：向 Git 平台回写提交状态）

10. ~~**模板与复用**：带输入参数的步骤模板（`uses` / `with`，创建运行时展开）、步骤级 `env`、内置与团队流水线模板~~（已完成，含模板版本历史与 `uses: name@N` 锁定；后续：跨租户共享模板）

11. ~~**保留策略**：产物按时长过期（Server 默认值 + 流水线覆盖，保留最近一次成功运行的产物）、按数量清理旧运行~~（已完成；后续：按租户配额、日志单独归档到对象存储）

12. ~~**Kubernetes 执行器**：每个 Job 一个 Pod（init 容器准备工作区、主容器执行 Step），取消 / 超时删除 Pod，镜像拉取失败快速失败，遗留 Pod 清理~~（已完成；后续：按 Job 指定资源与节点选择、Pod 模板覆盖、Pod 事件写入日志）

13. ~~**对齐 Code Flow**：流水线成员 / 角色 / 公开、通用变量组与运行时变量、服务连接、内置步骤库与任务输出、运行条件、任务插件与审批消息、流水线 Webhook、并发上限与缓存目录、统计报表扩展、运行记录删除、分组与复制、账户安全（会话、API Key 有效期、删除账户）、构建节点（自建一键安装 / GitHub Actions 按需 / 主机 Agent）~~（已完成；Cloudreve 仅支持密码登录与本机存储策略，审批链接需配置 `FLOW_PUBLIC_URL`，缓存目录只用于容器任务）

后续方向：日志推送改用 PostgreSQL LISTEN/NOTIFY（多实例即时推送）、LDAP / OAuth 登录、缓存放到对象存储。
