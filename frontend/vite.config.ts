import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// Vite 配置:
//  - React 插件
//  - 开发服务器把这几类请求代理到后端 (默认 http://localhost:8080)
//
// 抽出 target 是为了四个代理共用一份配置,改后端地址时只改一处
const target = process.env.VITE_API_TARGET || 'http://localhost:8080';

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      // 业务接口:后端统一挂在 /api/v1 下
      '/api': { target, changeOrigin: true },
      // 以下三个是后端在**根路径**上托管的静态文件,不在 /api 下。
      // 不代理的话,<img src="/covers/..."> 会被 dev server 当成本地资源去找,直接 404。
      // 对应 main.go 里的 r.Static: /covers(封面)、/avatars(头像)、/videos(视频文件)
      '/covers': { target, changeOrigin: true },
      '/avatars': { target, changeOrigin: true },
      '/videos': { target, changeOrigin: true },
    },
  },
});