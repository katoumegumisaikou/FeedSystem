import { useEffect } from 'react';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { App as AntdApp } from 'antd';
import type { InternalAxiosRequestConfig } from 'axios';
import { MemoryRouter } from 'react-router-dom';
import http, { setGlobalMessage } from '../../api/axios';
import LoginPage from './index';

// 这条用例要证明的是**屏幕上弹出几条提示**,不是"某个函数被调到了几次"。
//
// 为什么不 mock api/auth:要证明的恰恰是"页面不要再补一条" —— 而"两条"这件事只有在
// **真的** axios 拦截器跑起来时才成立。所以这里换掉 axios 实例的 adapter
// (与 api/feed.test.ts / api/follow.test.ts 同一套做法),让真实链路走完:
//   adapter 回 HTTP 200 + code 10002 → 拦截器弹一条(它才是"失败要说的话")→ 页面若再弹一条,
//   屏幕上就是两条堆在一起。
//
// 为什么本文件里才做得到:router.test.tsx 把 api/auth 整个 mock 掉了,那条链路上拦截器永远不跑。

const FUTURE_FLAGS = { v7_startTransition: true, v7_relativeSplatPath: true } as const;

// 与 App.tsx 同一条接线:把 antd 的 message 注入 axios 的拦截器(它是模块级代码,用不了 App.useApp)。
// 少了这一条,拦截器那句提示不会上屏,本用例就退化成"只数页面自己那一条"。
function MessageBridge() {
  const { message } = AntdApp.useApp();
  useEffect(() => {
    setGlobalMessage(message);
  }, [message]);
  return null;
}

// antd 的表单页会经 Grid 的断点观察器用到 matchMedia(与 router.test.tsx 同一处 stub)
window.matchMedia = ((query: string) => ({
  matches: false,
  media: query,
  onchange: null,
  addListener: () => {},
  removeListener: () => {},
  addEventListener: () => {},
  removeEventListener: () => {},
  dispatchEvent: () => false,
})) as unknown as typeof window.matchMedia;

/** 让真实的 login() 走真实拦截器:后端"未登录"的通用写法是 HTTP 200 + body.code(见 response.Error)。 */
function rejectAsUnauthorized() {
  http.defaults.adapter = (config: InternalAxiosRequestConfig) =>
    Promise.resolve({
      data: { code: 10002, msg: '未登录' },
      status: 200,
      statusText: 'OK',
      headers: {},
      config,
    });
}

function renderLogin() {
  return render(
    <AntdApp>
      <MemoryRouter initialEntries={['/login']} future={FUTURE_FLAGS}>
        <MessageBridge />
        <LoginPage />
      </MemoryRouter>
    </AntdApp>,
  );
}

async function submitLogin() {
  // fireEvent.change 而不是 userEvent.type:本用例关心的是"提交后弹几条提示",
  // 一个字符一个字符地敲(antd 表单每键一次重渲染)只会让用例慢到超时。
  fireEvent.change(screen.getByLabelText('手机号'), { target: { value: '13800000000' } });
  fireEvent.change(screen.getByLabelText('密码'), { target: { value: 'abc12345' } });
  await userEvent.click(screen.getByRole('button', { name: /登\s*录/ }));
}

describe('登录失败时的错误提示', () => {
  it('只弹拦截器那一条:页面自己不再补一条(堆两条提示是重复告知)', async () => {
    rejectAsUnauthorized();

    renderLogin();
    await submitLogin();

    // 失败要说的话由拦截器给(它念的是后端返回的 msg)—— 这条先成立,证明这次失败真的走完了链路:
    // 屏幕上已经有一条提示了。少了这一条,下面的"只有一条"就可能是"一条都没有"的假绿灯。
    expect(await screen.findByText('未登录')).toBeInTheDocument();

    // 页面自己那句"登录失败"必须不存在(它与拦截器说的是同一件事,只是更笼统)
    expect(screen.queryByText('登录失败')).toBeNull();

    await waitFor(() => {
      expect(document.querySelectorAll('.ant-message-notice')).toHaveLength(1);
    });
    // 失败之后停在登录页,让用户改密码重试
    expect(screen.getByRole('heading', { name: '登录' })).toBeInTheDocument();
  });
});
