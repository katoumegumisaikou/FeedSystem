import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { App as AntdApp } from 'antd';
import { MemoryRouter } from 'react-router-dom';
import type { FeedListProps } from './components/feed/FeedList';
import { getMyProfile, login } from './api/auth';
import { listFollowingVideos, listHotVideos, listLatestVideos } from './api/feed';
import { AppRouter } from './router';
import type { UserResp } from './types/auth';
import type { FeedCursor, FeedItem } from './types/feed';

// 这个文件要证明的是**路由把人送到哪一页、那一页拉了哪一条流**,不是组件的内部实现。
//
// 因此:三个取数函数与账号接口一律 mock(HTTP 的路径 / 参数归 api/*.test.ts),
// "渲染出的到底是哪一页"看两件外部可见的事 —— 这一页自己的空态文案(空流时屏幕上就是它),
// 以及被要走的取数函数(三个流各有一个,游标形状还不同,拉错函数是会拼错参数的)。
//
// 文案是 spec §13 那张表的原话,逐字写在下面(而不是从页面里 import):组件的文案一旦被改掉,
// 这些用例就该红。
const HOME_EMPTY_TEXT = '还没有视频 · 去上传你的第一条';
const FOLLOWING_EMPTY_TEXT = '你还没有关注的人 · 去主页看看';

// —— 替身 1:接口 ——
vi.mock('./api/feed', () => ({
  listLatestVideos: vi.fn(),
  listHotVideos: vi.fn(),
  listFollowingVideos: vi.fn(),
}));

// 登录 / 探测。/login 落地页与"需登录"的守卫都读它。
vi.mock('./api/auth', () => ({
  getMyProfile: vi.fn(),
  login: vi.fn(),
}));

// 登录成功会作废「我关注了谁」的缓存(身份变了)。那份缓存本身有它自己的测试,
// 这里放行真实现的话,每个用例都会顺带发一次全量关注列表的请求 —— 与路由无关的噪声。
vi.mock('./hooks/useFollowees', () => ({
  invalidateFollowees: vi.fn(),
  useFollowees: () => ({ ids: new Set<number>(), loading: false, refresh: () => {} }),
  setFollowee: vi.fn(),
}));

const getMyProfileMock = vi.mocked(getMyProfile);
const loginMock = vi.mocked(login);
const listLatestVideosMock = vi.mocked(listLatestVideos);
const listHotVideosMock = vi.mocked(listHotVideos);
const listFollowingVideosMock = vi.mocked(listFollowingVideos);

// —— 替身 2:媒体播放 ——
// 这个文件里会有一条用例真的把视频渲染出来(证明页面与容器的接线)。jsdom 里
// HTMLMediaElement.play / pause 只往虚拟控制台扔一条 "not implemented",换掉只是为了安静。
const realPlay = HTMLMediaElement.prototype.play;
const realPause = HTMLMediaElement.prototype.pause;

// react-router v6 的两个 v7 预告开关(与 MainLayout / FeedList / ActionRail 的测试同一份):
// 不打开它们,每次渲染都会往 stderr 打两行 v7 预告,把测试输出弄脏。
const FUTURE_FLAGS = { v7_startTransition: true, v7_relativeSplatPath: true } as const;

const ME: UserResp = {
  id: 42,
  user_name: 'ami',
  phone: '13800000000',
  created_at: '2026-09-12T16:50:53.025052Z',
};

// 四态文案(spec §13 那张表的原话),逐字写在下面:守卫自己那份一旦被改掉,这些用例就该红。
const LOADING_TEXT = '加载中…';
const ERROR_TEXT = '加载失败,点此重试';

/** 探测 401 = 游客。**注意外壳(MainLayout)与守卫都会各探一次** ——
 *  断言不能写成"只调了一次",那是实现细节,不是行为。
 *
 *  为什么失败对象要写成这个样子:守卫现在**按失败种类分类**(只有 401 / 业务码 10002 才算游客)。
 *  原来的 `new Error('401')` 是个"什么失败都行"的替身,在新行为下它恰好代表"探测失败"
 *  (消息里写 401 没用 —— 判定看的是 HTTP 状态与业务码,不是错误消息文本)。 */
function asGuest() {
  getMyProfileMock.mockRejectedValue({ response: { status: 401 } });
}

/** 探测撞上 5xx / 网络故障 / 超时:这不是"没登录",守卫不许把人送去登录页。 */
function asProbeFailed(err: unknown = { response: { status: 500 } }) {
  getMyProfileMock.mockRejectedValue(err);
}

function asLoggedIn() {
  getMyProfileMock.mockResolvedValue(ME);
}

/** 一页到底的空流:容器立刻停在空态,不会去拉下一页 —— 也就不会给"发了几个请求"的断言加噪声。
 *  不指定成某一个响应的类型:三条流的响应是同构的(`{items, next_cursor?}`),只有游标形状不同,
 *  这里本来就没有游标可给,三个 mock 都能直接用。 */
const EMPTY_PAGE = { items: [] as FeedItem[], next_cursor: undefined };

const VIDEO: FeedItem = {
  id: 1,
  author_id: 7,
  username: 'megu',
  title: '第 1 条',
  play_url: '/videos/7/1.mp4',
  cover_url: '/covers/1/a.jpg',
  created_at: '2026-10-15T10:00:00Z',
  status: 1,
  play_count: 0,
  likes_count: 0,
  comment_count: 0,
};

// 记录**每次渲染**时 FeedList 收到的 load 引用 —— 用来证明页面给的是稳定引用(见文件末尾那条)。
// 包装而不是替换:其余用例跑的还是真正的容器,只是多了这一个观察点。
const loadProps = vi.hoisted(() => [] as unknown[]);

vi.mock('./components/feed/FeedList', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./components/feed/FeedList')>();
  return {
    default: function RecordingFeedList<C>(props: FeedListProps<C>) {
      loadProps.push(props.load);
      return actual.default(props);
    },
  };
});

// 与 main.tsx 同一层包装:antd 的 <App> 提供 message 的上下文。
// 登录成功会 message.success,少了这一层 App.useApp() 拿到的是空对象,
// 那一行会当场抛错 —— 测试就会挂在一个跟被测行为无关的地方。
function renderRouter(path: string = '/') {
  return render(
    <AntdApp>
      <MemoryRouter initialEntries={[path]} future={FUTURE_FLAGS}>
        <AppRouter />
      </MemoryRouter>
    </AntdApp>,
  );
}

function renderRouterAtLogin(state: unknown) {
  return render(
    <AntdApp>
      <MemoryRouter initialEntries={[{ pathname: '/login', state }]} future={FUTURE_FLAGS}>
        <AppRouter />
      </MemoryRouter>
    </AntdApp>,
  );
}

/** 登录页的标题(level=3 的 Title ⇒ <h3>)。用它识别"此刻在登录页" ——
 *  登录按钮也叫"登录",所以这里按 heading 找,不会和按钮混。 */
const loginHeading = () => screen.queryByRole('heading', { name: '登录' });

const feedRoot = () => screen.queryByTestId('feed-list-root');

/** 填表并提交。/登录按钮的可访问名是「登 录」—— antd 会在两个中文字之间插一个空格,
 *  所以这里用正则匹配,不把那个空格写死。 */
async function fillAndSubmitLogin() {
  fireEvent.change(screen.getByLabelText('手机号'), { target: { value: '13800000000' } });
  fireEvent.change(screen.getByLabelText('密码'), { target: { value: 'abc12345' } });
  await userEvent.click(screen.getByRole('button', { name: /登\s*录/ }));
}

// —— 替身 3:matchMedia ——
// jsdom 没实现它,而 antd 的表单页会经 Grid 的断点观察器用到它(缺了它,登录页一挂载就抛
// "window.matchMedia is not a function")。只在本文件里补:目前只有这个文件会渲染 antd 的表单页,
// 不必把整个测试基建拖进来(真要复用,该搬到 src/test/setup.ts)。
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

// 手动可控的 Promise:用来把"登录态还在探测"这个状态停住(守卫此刻什么都不该渲染)。
function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

beforeEach(() => {
  vi.resetAllMocks();
  // 引用是模块级的数组:不还原就会带着上一个用例的记录,引用相等就成了假的绿灯。
  loadProps.length = 0;

  HTMLMediaElement.prototype.play = vi.fn().mockResolvedValue(undefined) as unknown as typeof realPlay;
  HTMLMediaElement.prototype.pause = vi.fn() as unknown as typeof realPause;
});

afterEach(() => {
  HTMLMediaElement.prototype.play = realPlay;
  HTMLMediaElement.prototype.pause = realPause;
});

describe('三个流的路由接入', () => {
  it('游客访问 /:主页可进,挂在外壳里,拉的是最新流', async () => {
    asGuest();
    listLatestVideosMock.mockResolvedValue(EMPTY_PAGE);

    renderRouter('/');

    // 这一页自己的空态文案上屏 = 这一页真的渲染了(spec §13)
    expect(await screen.findByText(HOME_EMPTY_TEXT)).toBeInTheDocument();
    expect(listLatestVideosMock).toHaveBeenCalledTimes(1);
    // 嵌套路由的证明:外壳的导航在(三个流页面都挂在外壳下),而且没被送去登录页
    expect(screen.getByRole('navigation', { name: '主导航' })).toBeInTheDocument();
    expect(loginHeading()).not.toBeInTheDocument();
  });

  it('游客访问 /hot:热门可进,拉的是点赞流(不是最新流)', async () => {
    asGuest();
    listHotVideosMock.mockResolvedValue(EMPTY_PAGE);

    renderRouter('/hot');

    // spec §13 只给了"流里没视频"这一句通用空态文案,热门流没有自己那句
    expect(await screen.findByText(HOME_EMPTY_TEXT)).toBeInTheDocument();
    expect(listHotVideosMock).toHaveBeenCalledTimes(1);
    // 游标形状不同的两条流共用同一个函数就会拼错参数 —— 所以这里连"拉的是哪一个"都要钉住
    expect(listLatestVideosMock).not.toHaveBeenCalled();
    expect(loginHeading()).not.toBeInTheDocument();
  });

  it('游客访问 /following:被送回登录页,关注流一个请求都不发', async () => {
    asGuest();

    renderRouter('/following');

    await waitFor(() => expect(loginHeading()).toBeInTheDocument());
    // 不放行就不能放行到底:关注流的接口是强鉴权的,拿游客身份去拉只会换一个 401
    expect(listFollowingVideosMock).not.toHaveBeenCalled();
    expect(feedRoot()).not.toBeInTheDocument();
  });

  it('登录态还在探测时,/following 不放行、也不先闪一下登录页,但**有话说**', async () => {
    const probe = deferred<UserResp>();
    getMyProfileMock.mockReturnValue(probe.promise);

    renderRouter('/following');

    // 探测未回来:两边都不许出现 —— 既不能放出关注流(可能是游客),
    // 也不能先闪登录页(已登录的人会白看一眼登录表单)
    expect(feedRoot()).not.toBeInTheDocument();
    expect(loginHeading()).not.toBeInTheDocument();
    expect(listFollowingVideosMock).not.toHaveBeenCalled();

    // ...但也不许是一片黑:外壳是近黑底,"什么都不渲染"最多能黑到 axios 的 10s 超时。
    // 这一帧必须有一句"在加载"(role=status ⇒ 读屏也会听到,不是只有眼睛看得到)。
    expect(screen.getByRole('status')).toHaveTextContent(LOADING_TEXT);

    await act(async () => {
      probe.reject({ response: { status: 401 } });
    });

    await waitFor(() => expect(loginHeading()).toBeInTheDocument());
  });

  it('探测撞上 500:不跳登录页,给一个可重试的失败态;点了重试能放行', async () => {
    asProbeFailed();
    listFollowingVideosMock.mockResolvedValue(EMPTY_PAGE);

    renderRouter('/following');

    // 5xx / 网络故障 / 超时都**不是**"没登录" —— 把一个已登录的人送去登录页,
    // 他看到的是一个无法解释的跳转(而且登录页也会因为同一个原因失败)。
    expect(await screen.findByRole('button', { name: ERROR_TEXT })).toBeInTheDocument();
    expect(loginHeading()).not.toBeInTheDocument();
    expect(listFollowingVideosMock).not.toHaveBeenCalled();

    // 后端回来了:重试必须真的再探一次并放行(否则这个失败态就是个死胡同)
    asLoggedIn();

    await userEvent.click(screen.getByRole('button', { name: ERROR_TEXT }));

    expect(await screen.findByText(FOLLOWING_EMPTY_TEXT)).toBeInTheDocument();
    expect(listFollowingVideosMock).toHaveBeenCalledTimes(1);
  });

  it('业务码 10002(HTTP 200 + code)也算未登录 ⇒ 送登录页', async () => {
    // 后端 handler / service 层的错误走 response.Error:HTTP 200 + body.code。
    // 只看 HTTP 状态会漏掉这一种,而它说的就是"未登录"(与中间件那条 401 同一件事)。
    asProbeFailed({ code: 10002 });

    renderRouter('/following');

    await waitFor(() => expect(loginHeading()).toBeInTheDocument());
    expect(screen.queryByRole('button', { name: ERROR_TEXT })).toBeNull();
  });

  it('已登录访问 /following:守卫放行,拉的是关注流', async () => {
    asLoggedIn();
    listFollowingVideosMock.mockResolvedValue(EMPTY_PAGE);

    renderRouter('/following');

    expect(await screen.findByText(FOLLOWING_EMPTY_TEXT)).toBeInTheDocument();
    expect(listFollowingVideosMock).toHaveBeenCalledTimes(1);
    expect(loginHeading()).not.toBeInTheDocument();
  });
});

describe('登录的落地页', () => {
  it('直接进登录页:登录成功后落在主页 /(不再是个人主页)', async () => {
    asGuest();
    loginMock.mockResolvedValue({ logged_in: true });
    listLatestVideosMock.mockResolvedValue(EMPTY_PAGE);

    renderRouter('/login');
    await fillAndSubmitLogin();

    expect(await screen.findByText(HOME_EMPTY_TEXT)).toBeInTheDocument();
    expect(listLatestVideosMock).toHaveBeenCalledTimes(1);
    expect(loginHeading()).not.toBeInTheDocument();
  });

  it('从 /following 被送进登录页:登录成功后回到 /following,而不是主页', async () => {
    asGuest();
    loginMock.mockResolvedValue({ logged_in: true });
    listFollowingVideosMock.mockResolvedValue(EMPTY_PAGE);

    renderRouter('/following');
    await waitFor(() => expect(loginHeading()).toBeInTheDocument());

    // 登录成功之后 cookie 才落地,守卫那一次**新的**探测就会通过 —— 这里换成"已登录",
    // 模拟的正是那一次真实探测(客户端没有会话状态,登录态的唯一事实来源就是它)。
    asLoggedIn();

    await fillAndSubmitLogin();

    expect(await screen.findByText(FOLLOWING_EMPTY_TEXT)).toBeInTheDocument();
    expect(listFollowingVideosMock).toHaveBeenCalledTimes(1);
    // 没有落到主页:回的是原址,不是默认落地页
    expect(listLatestVideosMock).not.toHaveBeenCalled();
  });

  it('登录页只认站内相对原址:绝对地址不会把用户带走', async () => {
    asGuest();
    loginMock.mockResolvedValue({ logged_in: true });
    listLatestVideosMock.mockResolvedValue(EMPTY_PAGE);

    renderRouterAtLogin({ from: '//evil.example/x' });
    await fillAndSubmitLogin();

    // 站外地址被丢掉 ⇒ 走默认落地页(仍然停在站内)
    expect(await screen.findByText(HOME_EMPTY_TEXT)).toBeInTheDocument();
  });
});

describe('页面给容器的 load 是稳定引用', () => {
  it('重渲染之后 load 还是同一个引用(内联箭头每次都是新的),而且没有多发请求', async () => {
    asGuest();
    listLatestVideosMock.mockResolvedValue(EMPTY_PAGE);

    const { rerender } = renderRouter('/');
    await screen.findByText(HOME_EMPTY_TEXT);

    const before = loadProps.length;
    expect(before).toBeGreaterThan(0);

    for (let i = 0; i < 3; i += 1) {
      rerender(
        <AntdApp>
          <MemoryRouter initialEntries={['/']} future={FUTURE_FLAGS}>
            <AppRouter />
          </MemoryRouter>
        </AntdApp>,
      );
    }

    // 这几下确实把页面又渲染了几遍(否则下面的"引用没变"是空的,断言不成立)
    expect(loadProps.length).toBeGreaterThan(before);

    // 头号断言:每一次都必须是**同一个函数**。页面里写成内联箭头的话这里每次都是新引用 ——
    // 而 FeedList 把 load 放进 useCallback 的依赖里,引用一变那个 effect 就重跑(可能多发一次尾页请求)。
    const [first, ...rest] = loadProps.slice(before);
    expect(typeof first).toBe('function');
    rest.forEach((fn) => expect(fn).toBe(first));

    // 引用变了 effect 就重跑,而重跑的最终后果就是多一次请求 —— 顺带钉住"总共只有一次"
    expect(listLatestVideosMock).toHaveBeenCalledTimes(1);
  });

  it('适配器把 next_cursor 翻译成容器要的 nextCursor,游标原样递回去', async () => {
    asGuest();
    const cursor: FeedCursor = { created_at: 1_700_000_000_000_000, video_id: 42 };
    listLatestVideosMock.mockResolvedValue({ items: [VIDEO], next_cursor: cursor });

    renderRouter('/');
    await screen.findByTestId('video-panel');

    // 直接拿页面交给容器的那个函数来调 —— 容器下一页就是这么调的(哨兵驱动的那一层由 FeedList
    // 自己的测试负责,这里只证明接口的形状对得上)。
    const load = loadProps[loadProps.length - 1] as FeedListProps<FeedCursor>['load'];
    await expect(load(cursor)).resolves.toEqual({ items: [VIDEO], nextCursor: cursor });
    expect(listLatestVideosMock).toHaveBeenLastCalledWith(cursor);
  });
});

describe('满屏出血:页面不缩视频,避让只落在浮层 / 文字上', () => {
  it('页面盒子不带任何内缩:视频仍铺满可视区,导航浮在它上面', async () => {
    asGuest();
    listLatestVideosMock.mockResolvedValue({ items: [VIDEO], next_cursor: undefined });

    renderRouter('/');

    // 页面一旦整体内缩(margin / padding),**视频本身**就被挤窄 —— 那不再是"铺满可视区"
    // (spec §7/§12 与"不能把视频本身缩窄"这条约束)。避让只能落在浮在视频上的东西上:
    // VideoPanel 给左下角文字留的左内边距(见 VideoPanel.test.tsx 那条)。
    expect(screen.getByTestId('feed-page').getAttribute('style')).toBeNull();

    // 面板仍是整宽的一屏 —— 没有被页面推窄
    expect((await screen.findByTestId('video-panel')).style.width).toBe('100%');
  });
});

describe('页面与容器的接线', () => {
  it('主页把适配器返回的条目交给容器,一屏一个面板', async () => {
    asGuest();
    listLatestVideosMock.mockResolvedValue({ items: [VIDEO], next_cursor: undefined });

    renderRouter('/');

    expect(await screen.findByTestId('video-panel')).toBeInTheDocument();
    expect(screen.getByText('第 1 条')).toBeInTheDocument();
    // 右侧操作栏从面板的槽位进去(组合点在 FeedList,这里只证明这一页真的把它渲染出来了)
    expect(screen.getByTestId('action-rail')).toBeInTheDocument();
    expect(listLatestVideosMock).toHaveBeenCalledTimes(1);
  });
});
