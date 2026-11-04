import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// base 设为根：文档站走自定义域名 gsh.zgric.top（GitHub Pages + 自定义域名）。
export default defineConfig({
  plugins: [react()],
  base: '/',
  build: { outDir: 'dist' },
})
