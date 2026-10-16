import './index.css';
// feed 面上的样式表规则(焦点环 / 悬停 / 按下 / 滚动条):这四类都要伪类、伪元素或媒体查询,
// inline style 表达不了。挂在 .feed-surface(MainLayout 的根)与 .feed-scroller(FeedList 的
// 滚动容器)上,只影响 feed 这棵树,登录 / 注册 / 个人主页那些浅色页一个像素都不动。
import './feed.css';
import React from 'react';
import ReactDOM from 'react-dom/client';
import { BrowserRouter } from 'react-router-dom';
import { App as AntdApp, ConfigProvider } from 'antd';
import zhCN from 'antd/locale/zh_CN';
import App from './App';

// AntdApp 提供上下文感知的 message 实例,App.tsx 靠它把 message 注入 axios 拦截器
ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <ConfigProvider locale={zhCN}>
      <AntdApp>
        <BrowserRouter>
          <App />
        </BrowserRouter>
      </AntdApp>
    </ConfigProvider>
  </React.StrictMode>,
);