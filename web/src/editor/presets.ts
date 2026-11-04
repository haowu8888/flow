// 任务组件库：常见构建 / 测试 / 镜像 / 部署任务的起点，选中后生成可继续编辑的任务定义。
import { parse as parseYaml } from 'yaml'
import type { Template } from '../types'

export interface Preset {
  id: string
  category: string
  title: string
  description: string
  icon: string
  /** 任务名的基础（重名时自动加序号）。 */
  name: string
  job: Record<string, unknown>
}

export const CATEGORIES = ['构建', '测试', '主机部署', '镜像与容器', '人工卡点', '通用'] as const

const step = (name: string, run: string) => ({ name, run })

const gate = (id: string, title: string, description: string, icon: string): Preset => ({
  id,
  category: '人工卡点',
  title,
  description,
  icon,
  name: 'approve',
  job: { type: 'approval', notify: [{ connection: '' }] },
})

export const PRESETS: Preset[] = [
  {
    id: 'gate',
    category: '人工卡点',
    title: '人工审批',
    description: '暂停流水线，等成员在 Flow 中通过或驳回',
    icon: '✋',
    name: 'approve',
    job: { type: 'approval' },
  },
  gate('gate-email', '邮件审批', '暂停任务，发邮件给审批人，点邮件里的链接通过或驳回', '✉'),
  gate('gate-bark', 'Bark 审批', '暂停任务，推送 Bark 给审批人，点通知里的链接审批', 'B'),
  gate('gate-telegram', 'Telegram 审批', '暂停任务，往 Telegram 会话发带「通过 / 驳回」按钮的消息', 'T'),
  {
    id: 'go-build',
    category: '构建',
    title: 'Go 构建',
    description: '下载依赖并编译，二进制作为产物上传',
    icon: 'Go',
    name: 'go-build',
    job: {
      image: 'golang:1.23',
      cache: { key: 'go-mod', paths: ['.gomodcache'] },
      artifacts: ['bin'],
      steps: [step('build', 'export GOMODCACHE="$PWD/.gomodcache"\ngo mod download\ngo build -o bin/ ./...\n')],
    },
  },
  {
    id: 'node-build',
    category: '构建',
    title: 'Node.js 构建',
    description: 'npm ci 安装依赖后执行 build，dist 作为产物',
    icon: 'JS',
    name: 'node-build',
    job: {
      image: 'node:20',
      cache: { key: 'npm', paths: ['.npm'] },
      artifacts: ['dist'],
      steps: [step('install', 'npm ci --cache .npm'), step('build', 'npm run build')],
    },
  },
  {
    id: 'maven-build',
    category: '构建',
    title: 'Java Maven 构建',
    description: 'mvn package（跳过测试），jar 作为产物',
    icon: 'Jv',
    name: 'maven-build',
    job: {
      image: 'maven:3.9-eclipse-temurin-21',
      cache: { key: 'm2', paths: ['.m2'] },
      artifacts: ['target/*.jar'],
      steps: [step('package', 'mvn -B -Dmaven.repo.local=.m2 -DskipTests package')],
    },
  },
  {
    id: 'python-build',
    category: '构建',
    title: 'Python 打包',
    description: '安装依赖并构建 wheel',
    icon: 'Py',
    name: 'python-build',
    job: {
      image: 'python:3.12',
      cache: { key: 'pip', paths: ['.pip-cache'] },
      artifacts: ['dist'],
      steps: [step('build', 'pip install --cache-dir .pip-cache build\npython -m build\n')],
    },
  },
  {
    id: 'go-test',
    category: '测试',
    title: 'Go 单元测试',
    description: 'go vet 与 go test（含竞态检测）',
    icon: 'Go',
    name: 'go-test',
    job: {
      image: 'golang:1.23',
      cache: { key: 'go-mod', paths: ['.gomodcache'] },
      reports: { junit: ['reports/junit.xml'] },
      steps: [
        step(
          'test',
          'export GOMODCACHE="$PWD/.gomodcache"\ngo vet ./...\ngo install gotest.tools/gotestsum@latest\nmkdir -p reports\n"$(go env GOPATH)/bin/gotestsum" --junitfile reports/junit.xml -- -race ./...\n',
        ),
      ],
    },
  },
  {
    id: 'node-test',
    category: '测试',
    title: 'Node.js 测试',
    description: 'npm ci 后执行 lint 与 test',
    icon: 'JS',
    name: 'node-test',
    job: {
      image: 'node:20',
      cache: { key: 'npm', paths: ['.npm'] },
      steps: [step('install', 'npm ci --cache .npm'), step('lint', 'npm run lint --if-present'), step('test', 'npm test')],
    },
  },
  {
    id: 'maven-test',
    category: '测试',
    title: 'Java Maven 测试',
    description: 'mvn verify，运行单元测试与集成测试',
    icon: 'Jv',
    name: 'maven-test',
    job: {
      image: 'maven:3.9-eclipse-temurin-21',
      cache: { key: 'm2', paths: ['.m2'] },
      reports: { junit: ['target/surefire-reports/*.xml'] },
      steps: [step('verify', 'mvn -B -Dmaven.repo.local=.m2 verify')],
    },
  },
  {
    id: 'pytest',
    category: '测试',
    title: 'Python pytest',
    description: '安装 requirements.txt 后运行 pytest',
    icon: 'Py',
    name: 'pytest',
    job: {
      image: 'python:3.12',
      cache: { key: 'pip', paths: ['.pip-cache'] },
      reports: { junit: ['reports/junit.xml'] },
      steps: [step('test', 'pip install --cache-dir .pip-cache -r requirements.txt pytest\npytest --junitxml=reports/junit.xml\n')],
    },
  },
  {
    id: 'docker-image',
    category: '镜像与容器',
    title: 'Docker 镜像构建推送',
    description: '构建镜像并推送到仓库；Runner 需能访问 Docker，登录凭据放在密钥 REGISTRY_USER / REGISTRY_PASSWORD',
    icon: 'Dk',
    name: 'docker-image',
    job: {
      image: 'docker:27-cli',
      env: { IMAGE: 'registry.example.com/team/app' },
      steps: [
        step('login', 'echo "$REGISTRY_PASSWORD" | docker login "${IMAGE%%/*}" -u "$REGISTRY_USER" --password-stdin'),
        step('build', 'docker build -t "$IMAGE:$FLOW_RUN_ID" .'),
        step('push', 'docker push "$IMAGE:$FLOW_RUN_ID"'),
      ],
    },
  },
  {
    id: 'k8s-deploy',
    category: '镜像与容器',
    title: 'Kubernetes 部署',
    description: '更新 Deployment 镜像并等待滚动完成；kubeconfig 放在密钥 KUBECONFIG_DATA',
    icon: 'K8s',
    name: 'k8s-deploy',
    job: {
      image: 'bitnami/kubectl:1.31',
      env: { NAMESPACE: 'default', DEPLOYMENT: 'app', IMAGE: 'registry.example.com/team/app' },
      steps: [
        step(
          'deploy',
          'echo "$KUBECONFIG_DATA" > kubeconfig\nexport KUBECONFIG="$PWD/kubeconfig"\nkubectl -n "$NAMESPACE" set image "deployment/$DEPLOYMENT" "*=$IMAGE:$FLOW_RUN_ID"\nkubectl -n "$NAMESPACE" rollout status "deployment/$DEPLOYMENT" --timeout=5m\n',
        ),
      ],
    },
  },
  {
    id: 'host-deploy',
    category: '主机部署',
    title: '部署到主机组（通用）',
    description: '把上游产物上传到主机组的每台服务器并执行脚本，支持分批、审批',
    icon: '⇪',
    name: 'deploy',
    job: {
      deploy: { hosts: '', upload: 'dist', target: '/opt/apps/app' },
      steps: [step('deploy', '# 当前目录就是上传后的远程目录\necho "deploying to $FLOW_DEPLOY_HOST"\nls -la\n')],
    },
  },
  {
    id: 'jar-deploy',
    category: '主机部署',
    title: 'Java 应用部署（jar）',
    description: '上传 target 下的 jar，停止旧进程后后台启动并检查',
    icon: 'Jv',
    name: 'deploy-java',
    job: {
      deploy: { hosts: '', upload: 'target', target: '/opt/apps/app' },
      steps: [
        step(
          'restart',
          'APP=$(ls *.jar | grep -v original | head -1)\npkill -f "java -jar $FLOW_DEPLOY_DIR/" || true\nsleep 2\nnohup java -jar "$FLOW_DEPLOY_DIR/$APP" > app.log 2>&1 &\nsleep 5\npgrep -f "$FLOW_DEPLOY_DIR/$APP" > /dev/null || { tail -50 app.log; exit 1; }\n',
        ),
      ],
    },
  },
  {
    id: 'static-deploy',
    category: '主机部署',
    title: '静态网站部署（Nginx）',
    description: '上传前端构建产物到站点目录并重载 Nginx',
    icon: 'Ngx',
    name: 'deploy-web',
    job: {
      deploy: { hosts: '', upload: 'dist', target: '/usr/share/nginx/html/app' },
      steps: [step('reload', 'nginx -t && nginx -s reload\n')],
    },
  },
  {
    id: 'systemd-deploy',
    category: '主机部署',
    title: '二进制服务部署（systemd）',
    description: '上传可执行文件并重启 systemd 服务',
    icon: 'Sd',
    name: 'deploy-service',
    job: {
      deploy: { hosts: '', upload: 'bin', target: '/opt/apps/app' },
      env: { SERVICE: 'app' },
      steps: [step('restart', 'sudo systemctl restart "$SERVICE"\nsleep 2\nsystemctl is-active "$SERVICE"\n')],
    },
  },
  {
    id: 'docker-run-deploy',
    category: '主机部署',
    title: 'Docker 容器部署',
    description: '在主机上拉取镜像并以新版本重建容器',
    icon: 'Dk',
    name: 'deploy-container',
    job: {
      deploy: { hosts: '', target: '/opt/apps/app' },
      env: { IMAGE: 'registry.example.com/team/app:latest', CONTAINER: 'app', PORTS: '8080:8080' },
      steps: [
        step(
          'run',
          'docker pull "$IMAGE"\ndocker rm -f "$CONTAINER" 2>/dev/null || true\ndocker run -d --name "$CONTAINER" --restart=always -p "$PORTS" "$IMAGE"\n',
        ),
      ],
    },
  },
  {
    id: 'shell',
    category: '通用',
    title: 'Shell 脚本',
    description: '在 Runner 主机上直接执行命令（可再指定镜像）',
    icon: '>_',
    name: 'job',
    job: { steps: [step('step-1', 'echo hello')] },
  },
  {
    id: 'approval',
    category: '通用',
    title: '人工卡点',
    description: '上游完成后暂停，等待指定成员审批通过再继续',
    icon: '✋',
    name: 'approval',
    job: { type: 'approval' },
  },
  {
    id: 'webhook',
    category: '通用',
    title: 'HTTP 回调',
    description: '用 curl 调用外部接口，例如通知发布系统',
    icon: 'API',
    name: 'webhook',
    job: {
      image: 'curlimages/curl:8.10.1',
      env: { URL: 'https://example.com/hook' },
      steps: [step('call', 'curl -fsS -X POST -H "Content-Type: application/json" -d "{\\"run\\":\\"$FLOW_RUN_ID\\"}" "$URL"')],
    },
  },
]

/** 团队步骤模板包装成任务：一个引用模板的步骤，必填输入预留为空。 */
export function templatePreset(t: Template): Preset {
  let inputs: Record<string, { required?: boolean }> = {}
  try {
    inputs = (parseYaml(t.yaml) as { inputs?: typeof inputs })?.inputs ?? {}
  } catch {
    // 模板 YAML 已由服务端校验，解析失败时不预填输入
  }
  const required = Object.entries(inputs).filter(([, v]) => v?.required).map(([k]) => [k, ''])
  return {
    id: `template:${t.id}`,
    category: '团队模板',
    title: t.name,
    description: t.description || '团队步骤模板',
    icon: '◆',
    name: t.name,
    job: { steps: [{ name: t.name, uses: t.name, ...(required.length ? { with: Object.fromEntries(required) } : {}) }] },
  }
}
