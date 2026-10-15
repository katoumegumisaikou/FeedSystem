// 测试环境的一次性准备,由 vitest.config.ts 的 setupFiles 调用。
// 只做一件事:注册 @testing-library/jest-dom 的断言(toBeInTheDocument / toBeDisabled 等)。
// 走 '/vitest' 入口而不是裸包名 —— 前者同时挂上 vitest 的 expect 与类型声明。
import '@testing-library/jest-dom/vitest';
