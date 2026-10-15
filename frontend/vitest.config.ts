import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';

// 测试专用配置。与 vite.config.ts 分开,是为了让 `npm test` 不去加载 dev server 的 proxy 配置,
// 同时又能复用同一个 React 插件(JSX 转换规则必须和构建时一致,否则测的不是线上那份代码)。
export default defineConfig({
  plugins: [react()],
  test: {
    // 组件测试要真实 DOM:jsdom 提供 document / window 等浏览器 API
    environment: 'jsdom',
    // 打开后 describe / it / expect 无需 import,全项目统一写法;
    // 类型来自 tsconfig.json 的 "types": ["vitest/globals"]
    globals: true,
    // 每个测试文件运行前先执行,用于注册 jest-dom 匹配器(toBeInTheDocument 等)
    setupFiles: ['src/test/setup.ts'],
  },
});
