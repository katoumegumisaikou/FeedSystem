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
| `VITE_API_TARGET` | `http://localhost:8080` | dev 模式下代理的目标后端地址 |

## 开发代理(vite.config.ts)

dev server 一共代理**四类**路径,不只是业务接口:

| 路径 | 说明 |
|------|------|
| `/api` | 业务接口(后端统一挂在 `/api/v1` 下) |
| `/covers` | 视频封面。**feed 的 `<video poster>` 靠它**,视频流里的封面全是这个路径 |
| `/avatars` | 用户头像(右侧操作栏那个圆头像) |
| `/videos` | 视频文件本身。**feed 的 `<video src>` 靠它**,不代理就是一片黑 |

后三类是后端在**根路径**上托管的静态文件(见 `main.go` 的 `r.Static`),不在 `/api` 下。
不代理的话,`<img src="/covers/...">` 会被 dev server 当成本地资源去找,直接 404 ——
现象是"视频能播但封面全裂" / "点开就没有画面"。

## 目录结构

```
src/
├── api/           # axios 实例(拦截器 / silent 开关 / isUnauthorized)+ 各模块 API
├── components/    # 通用组件(SmsCodeButton 等)
│   └── feed/      # 视频流专用:FeedList / VideoPanel / ActionRail / FollowButton / icons
├── hooks/         # useInView(元素是否在视口)/ useFollowees(我关注了谁)
├── layouts/       # 布局壳(MainLayout 深色外壳 / AuthLayout 浅色卡片)
├── pages/         # 页面(Home / Hot / Following / Login / Register / ForgotPassword / UserProfile)
├── theme/         # tokens.ts:深色 token、间距、palette(手写样式的颜色)、几何常量
├── types/         # 与后端 DTO 对齐的 TS 类型
├── feed.css       # feed 面上"只能用样式表写"的几条(焦点环 / 悬停 / 按下态 / 滚动条)
├── App.tsx        # 根组件(把 antd message 注入 axios 拦截器)
├── main.tsx       # 入口
└── router.tsx     # 路由表 + RequireLogin 守卫
```

## 路由

| 路径 | 页面 | 鉴权 |
|------|------|------|
| `/` | 主页(最新视频流) | 否 |
| `/hot` | 热门(按点赞数排序) | 否 |
| `/following` | 关注流 | 是(游客 → `/login`,登录后回原页) |
| `/login` | 手机号 + 密码登录(成功后落地 `/`,被守卫送来时回原页) | 否 |
| `/register` | 注册(用户名 / 手机号 / 验证码 / 密码) | 否 |
| `/forgot-password` | 忘记密码(SMS 验证码流程) | 否 |
| `/users/:id` | 个人主页 | 否 |

`/`、`/hot`、`/following` 挂在 `layouts/MainLayout` 下(嵌套路由,左侧浮层导航 + 全屏视频流);
账号页与个人主页不进外壳。认不出来的路径回 `/`。

## 与后端的对齐

- 所有 API 走 `/api/v1/...`,路径与后端 `backend/internal/model/account/router.go` 一致
- 响应格式 `{ code, msg, data }`,错误统一在 axios 拦截器里弹 antd `message`
- **「未登录」有两种落地形态**,前端只认这两条(其余失败一律**不当**游客,见下):
  ① 中间件 `abortUnauthorized` 直接给 **HTTP 401**(`/users/me` 这类 SetSensitive 路由无 token 时);
  ② handler / service 层返回 `errs.ErrUnauthorized`,经 `response.Error` 落成 **HTTP 200 + `code: 10002`**
  (所以拦截器会给业务错误带上 `code`,见 `ApiError`)。
  判定收在 `api/axios.ts` 的 `isUnauthorized()` 一处:路由守卫(`RequireLogin`)与关注按钮共用。
  **5xx / 网络故障 / 10s 超时不是"未登录"** —— 守卫会停在一个可重试的失败态,而不是把人送去登录页。
- token 由后端通过 HttpOnly cookie 下发(`access_token` / `refresh_token`),前端不存 localStorage
- axios 实例开启 `withCredentials: true`,跨端口发送 cookie
- 校验规则(密码强度 / 用户名格式 / 手机号 / 验证码长度)与后端 DTO tag + service 校验一致