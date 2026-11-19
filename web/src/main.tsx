import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { App } from './App'
import './styles.css'

// 页面按需加载：发布新版本后旧的分块文件已不存在，打开中的页面切换路由会加载失败，此时刷新拿到新版本。
// 10 秒内刚刷新过仍失败（如网络断开）就不再刷新，交给页面的错误提示，避免循环刷新。
window.addEventListener('vite:preloadError', (e) => {
  const key = 'flow-chunk-reload'
  try {
    if (Date.now() - Number(sessionStorage.getItem(key) || 0) < 10_000) return
    sessionStorage.setItem(key, String(Date.now()))
  } catch {
    return
  }
  e.preventDefault()
  location.reload()
})

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
