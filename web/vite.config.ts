import { writeFileSync } from 'node:fs'
import { resolve } from 'node:path'
import react from '@vitejs/plugin-react'
import { defineConfig, type Plugin } from 'vite'

// 构建产物输出到 Go 包 internal/webui，由 go:embed 打进 Server 二进制。
const outDir = resolve(__dirname, '../internal/webui/dist')

// emptyOutDir 会清掉占位文件，补回来，保证未构建前端时 go:embed 仍能编译。
const keepPlaceholder = (): Plugin => ({
  name: 'keep-embed-placeholder',
  closeBundle() {
    writeFileSync(resolve(outDir, '.gitkeep'), '')
  },
})

const backend = process.env.FLOW_BACKEND ?? 'http://localhost:8080'

export default defineConfig({
  plugins: [react(), keepPlaceholder()],
  build: { outDir, emptyOutDir: true },
  server: { proxy: { '/api': backend, '/healthz': backend } },
})
