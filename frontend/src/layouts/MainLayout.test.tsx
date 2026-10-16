import { act, render, screen, waitFor } from '@testing-library/react';
import { ConfigProvider } from 'antd';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { getMyProfile } from '../api/auth';
import MainLayout from './MainLayout';

// api 一律 mock:这个文件要证明的是"外壳按登录态画入口",不测 HTTP ——
// /users/me 的路径与方法由 api/auth.ts 负责,不在这一层重复。
vi.mock('../api/auth', () => ({ getMyProfile: vi.fn() }));

const getMyProfileMock = vi.mocked(getMyProfile);

// 四个入口的文案(spec §6),逐字抄过来 —— 它们是对外契约的一部分。
const ENTRY_LABELS = ['主页', '热门', '关注', '个人主页'];

// react-router v6 的两个 v7 预告开关(与 ActionRail / FeedList 的测试同一份),见下面 MemoryRouter
const FUTURE_FLAGS = { v7_startTransition: true, v7_relativeSplatPath: true } as const;

/** @returns 某个入口那个 <a>(用 accessible name 找,顺带证明它有名字) */
const entry = (label: string) => screen.getByRole('link', { name: label });

// 外壳是"套在路由里"的组件:<Outlet/> 得有真实的子路由才算跑通,所以这里用
// MemoryRouter + 一条无 path 的布局路由包一层。三个子路由只是占位文本 ——
// 真正的三个流页面是 Task 10 的事,这里只证明"外壳是嵌套路由的宿主"。
function renderLayout(path = '/') {
  return render(
    // future 两个标记与 ActionRail / FeedList 的测试同一个写法:不打开它们,
    // react-router 每次渲染都会往 stderr 打两行 v7 预告,把测试输出弄脏。
    <MemoryRouter initialEntries={[path]} future={FUTURE_FLAGS}>
      <Routes>
        <Route element={<MainLayout />}>
          <Route path="/" element={<div>主页占位</div>} />
          <Route path="/hot" element={<div>热门占位</div>} />
          <Route path="/following" element={<div>关注占位</div>} />
        </Route>
      </Routes>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  getMyProfileMock.mockReset();
});

describe('MainLayout 的四个入口', () => {
  // 未登录是**最常发生的**那条路径(游客直接打开 /):探测请求会 401 回来。
  // 这条用例同时守着"401 不许把外壳弄坏、也不许把探测请求做成会弹提示的那种"。
  it('未登录:四个入口都在,「个人主页」指向 /login', async () => {
    getMyProfileMock.mockRejectedValue(new Error('401'));

    renderLayout();

    for (const label of ENTRY_LABELS) {
      expect(entry(label)).toBeInTheDocument();
    }
    // 探测确实发出去了;而且必须是 silent —— 游客没要求任何东西,不该被弹一句「未登录」
    expect(getMyProfileMock).toHaveBeenCalledWith({ silent: true });
    // 游客没有 id 可拼,只能去登录页(spec §8:游客点"个人主页"→ 去登录页)
    expect(entry('个人主页')).toHaveAttribute('href', '/login');

    // 让那条 401 真的落下来:rejection 是微任务,render 不等它 —— 不等就断言,
    // 测到的只是"探测还没跑完"的初值。这一拍之后才是"探测失败之后"的样子,
    // 断言它仍然是游客态:链接不许变成 /users/undefined。
    await act(async () => {});
    expect(entry('个人主页')).toHaveAttribute('href', '/login');
  });

  it('已登录:「个人主页」指向 /users/:id', async () => {
    getMyProfileMock.mockResolvedValue({
      id: 42,
      user_name: 'ami',
      phone: '13800000000',
      created_at: '2026-09-12T16:50:53.025052Z',
    });

    renderLayout();

    await waitFor(() => expect(entry('个人主页')).toHaveAttribute('href', '/users/42'));
    // 另外三个入口与登录态无关,一直是这三个目标
    expect(entry('主页')).toHaveAttribute('href', '/');
    expect(entry('热门')).toHaveAttribute('href', '/hot');
    expect(entry('关注')).toHaveAttribute('href', '/following');
  });

  it('内容区渲染子路由 —— 外壳是 <Outlet/> 的宿主', () => {
    getMyProfileMock.mockRejectedValue(new Error('401'));

    renderLayout('/hot');

    expect(screen.getByText('热门占位')).toBeInTheDocument();
  });
});

describe('MainLayout 的当前路由高亮', () => {
  it('在 /hot 时只有「热门」是当前页', () => {
    getMyProfileMock.mockRejectedValue(new Error('401'));

    renderLayout('/hot');

    // aria-current="page" 由 NavLink 自己给:读屏能听到"当前页",不只靠颜色
    expect(entry('热门')).toHaveAttribute('aria-current', 'page');
    // 「主页」的 / 必须精确匹配(end):否则它在**任何**路由下都算当前页,四个入口就分不出在哪
    expect(entry('主页')).not.toHaveAttribute('aria-current');
    expect(entry('关注')).not.toHaveAttribute('aria-current');
  });

  it('在 / 时「主页」是当前页', () => {
    getMyProfileMock.mockRejectedValue(new Error('401'));

    renderLayout('/');

    expect(entry('主页')).toHaveAttribute('aria-current', 'page');
    expect(entry('热门')).not.toHaveAttribute('aria-current');
  });
});

describe('MainLayout 的深色只作用于自己这棵子树', () => {
  // R1:深色 token 的接线点就在这个文件里(嵌套 ConfigProvider)。这条用例盯的是接线本身:
  // 父层一个浅色 ConfigProvider 都没影响到底下这个侧栏 —— 证明深色只刷了 feed 这棵树,
  // 登录 / 注册 / 个人主页那些浅色页不会被一起刷黑(spec §7)。
  it('父层是浅色时,侧栏仍取深色 token 的表面色', () => {
    getMyProfileMock.mockRejectedValue(new Error('401'));

    render(
      <ConfigProvider>
        <MemoryRouter future={FUTURE_FLAGS}>
          <MainLayout />
        </MemoryRouter>
      </ConfigProvider>,
    );

    // #16161c = rgb(22, 22, 28):深色 surface。父层浅色时的容器色是 #ffffff,不会是这个值
    expect(getComputedStyle(screen.getByRole('navigation')).backgroundColor).toBe('rgb(22, 22, 28)');
  });
});
