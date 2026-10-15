# Feed System Frontend

React + TypeScript + Vite + Ant Design 前端工程,对应后端 `feed-system/backend`。

## 启动

```bash
# 1. 安装依赖(需要 Node 20+)
npm install

# 2. 启动开发服务器(默认端口 5173)
#    /api 会被 vite proxy 转发到 VITE_API_TARGET(默认 http://localhost:8080)
npm run dev

# 3. 类型检查
npm run typecheck

# 4. 生产构建
npm run build

# 5. 跑测试(vitest + React Testing Library)
npm test
```

> **Node 版本:** 本工程要求 Node 20+(`package.json` 的 `engines.node: ">=20"`)。
> 测试基建的所有 devDependencies 均已固定在 Node 20 可用的大版本上
> (`jsdom@^26`、`@testing-library/jest-dom@^6`),不要在 Node 20 上升级它们的主版本。

## 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `VITE_API_TARGET` | `http://localhost:8080` | dev 模式下 `/api` 代理的目标后端地址 |

## 目录结构

```
src/
├── api/           # axios 实例 + auth API
├── components/    # 通用组件(SmsCodeButton 等)
├── layouts/       # 布局壳(AuthLayout 等)
├── pages/         # 页面(Login / Register / ForgotPassword 等)
├── types/         # 与后端 DTO 对齐的 TS 类型
├── App.tsx        # 根组件
├── main.tsx       # 入口
└── router.tsx     # 路由表
```

## 路由

| 路径 | 页面 | 鉴权 |
|------|------|------|
| `/login` | 手机号 + 密码登录 | 否 |
| `/login/sms` | 手机号 + 短信验证码登录 | 否 |
| `/register` | 注册(用户名 / 手机号 / 验证码 / 密码) | 否 |
| `/forgot-password` | 忘记密码(SMS 验证码流程) | 否 |
| `/` | 重定向到 `/login` | — |

## 与后端的对齐

- 所有 API 走 `/api/v1/...`,路径与后端 `backend/internal/model/account/router.go` 一致
- 响应格式 `{ code, msg, data }`,错误统一在 axios 拦截器里弹 antd `message`
- token 由后端通过 HttpOnly cookie 下发(`access_token` / `refresh_token`),前端不存 localStorage
- axios 实例开启 `withCredentials: true`,跨端口发送 cookie
- 校验规则(密码强度 / 用户名格式 / 手机号 / 验证码长度)与后端 DTO tag + service 校验一致