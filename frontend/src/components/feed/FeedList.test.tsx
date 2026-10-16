import { act, fireEvent, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { StrictMode, type ReactNode } from 'react';
import { MemoryRouter } from 'react-router-dom';
import type { FeedCursor, FeedItem } from '../../types/feed';
import FeedList from './FeedList';

// 这个文件要证明的核心只有一件事:**「到底」的唯一判据是游标缺失,不是 items 的长度**。
//
// 后端到底时 next_cursor 这个键**整个不发出来**(Go 的 *FeedCursor + omitempty)⇒ 前端拿到的是
// undefined;而 items 为空、或比 limit 短,都可能只是"这一页恰好没东西可给"(候选被截断、
// 窗口内条目已失效),游标之后确实还有内容。所以本文件里:
//   · 取数函数是**假的**(dispatch 要求)—— 测的是容器跟着游标走的逻辑,不是 HTTP;
//   · 翻页的断言看的是 load 被要走的**游标**,不是渲染出来的条数;
//   · "第一页空 + 游标 ⇒ 必须继续调 load(游标)"是头号断言,放在第一条。
//
// 文案是 spec §13 那张表的原话,逐字写在下面(而不是从组件里 import),
// 组件的文案一旦被改掉,这些用例就该红。

const LOADING_TEXT = '加载中…';
const ERROR_TEXT = '加载失败,点此重试';
const END_TEXT = '没有更多了';
const EMPTY_TEXT = '还没有视频 · 去上传你的第一条';
// Task 11 新增的一句:**没有 IntersectionObserver 的环境**下手动出口上的文案。
// 它不在 spec §13 那张四态表里(那张表假设"触底"这件事一定有人触发),所以逐字抄在这里,
// 组件那边一改,这条就该红。
const MORE_TEXT = '加载更多';

// —— 替身 1:IntersectionObserver ——
// 与 VideoPanel.test.tsx / hooks/useInView.test.ts 同一套写法:把回调和被观察元素交到测试手上,
// 测试就能像浏览器一样主动发一次「滚进 / 滚出视口」。本文件里"滑到底"全靠它驱动 ——
// jsdom 没有真实的滚动,也没有视口。
class FakeIntersectionObserver {
  static instances: FakeIntersectionObserver[] = [];

  callback: IntersectionObserverCallback;
  options?: IntersectionObserverInit;
  target: Element | null = null;

  constructor(callback: IntersectionObserverCallback, options?: IntersectionObserverInit) {
    this.callback = callback;
    this.options = options;
    FakeIntersectionObserver.instances.push(this);
  }

  observe(target: Element) {
    this.target = target;
  }

  disconnect() {}

  emit(isIntersecting: boolean) {
    this.callback(
      [{ isIntersecting, target: this.target } as unknown as IntersectionObserverEntry],
      this as unknown as IntersectionObserver,
    );
  }
}

// 哨兵那个 observer。**不能只取最后一个**:每个 VideoPanel 也会各自建一个 observer(它自己
// 管播放权),面板数量一变"最后一个"就会指错。按被观察的元素找,和面板数量无关。
function sentinelObserver(): FakeIntersectionObserver {
  const sentinel = screen.getByTestId('feed-list-sentinel');
  const observer = FakeIntersectionObserver.instances.find((o) => o.target === sentinel);
  if (!observer) throw new Error('哨兵没有被 IntersectionObserver 观察:触底加载就永远不会发生');
  return observer;
}

// 滑到底 / 滑回去。act 要用**异步**那种:IO 回调本身会驱动 setState,而它接力的那次请求
// 可能当场落地 —— 落地的 setState 必须落在这个 act 里面,否则 React 会给每一处打一条
// "not wrapped in act(...)" 警告(测试输出就不干净了)。
const atTail = () =>
  act(async () => {
    sentinelObserver().emit(true);
  });
const notAtTail = () =>
  act(async () => {
    sentinelObserver().emit(false);
  });

// —— 替身 2:媒体播放 ——
// jsdom 里 HTMLMediaElement.play / pause 只往虚拟控制台扔一条 "not implemented"。
// 本文件不测播放(那是 VideoPanel.test.tsx 的事),换掉只是为了面板能安静地挂上,
// 测试输出保持干净。
const realIntersectionObserver = globalThis.IntersectionObserver;
const realPlay = HTMLMediaElement.prototype.play;
const realPause = HTMLMediaElement.prototype.pause;

beforeEach(() => {
  FakeIntersectionObserver.instances = [];
  globalThis.IntersectionObserver = FakeIntersectionObserver as unknown as typeof IntersectionObserver;

  HTMLMediaElement.prototype.play = vi.fn().mockResolvedValue(undefined) as unknown as typeof realPlay;
  HTMLMediaElement.prototype.pause = vi.fn() as unknown as typeof realPause;
});

afterEach(() => {
  globalThis.IntersectionObserver = realIntersectionObserver;
  HTMLMediaElement.prototype.play = realPlay;
  HTMLMediaElement.prototype.pause = realPause;
});

// ActionRail(以及它里面的 FollowButton)会打真接口:点赞 / 全量关注列表。
// 这里的被测对象是容器,HTTP 由 api 层的测试负责;放行真请求会让"渲染完就断言"的用例
// 收到异步 setState,输出里多一堆 act 警告。注意:**load 是假的,不是 mock feed 的 api 层**。
vi.mock('../../api/video', () => ({
  likeVideo: vi.fn(),
  unlikeVideo: vi.fn(),
}));

vi.mock('../../hooks/useFollowees', () => ({
  useFollowees: () => ({ ids: new Set<number>(), loading: false, refresh: () => {} }),
  setFollowee: () => {},
}));

// ActionRail 的头像是个 <Link>,没有 Router 上下文就抛错。
// 提前打开 v7 future flag,免得 react-router v6 往 stderr 刷警告。
function TestRouter({ children }: { children: ReactNode }) {
  return (
    <MemoryRouter future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
      {children}
    </MemoryRouter>
  );
}

const CURSOR_A: FeedCursor = { created_at: 1_700_000_000_000_000, video_id: 42 };
const CURSOR_B: FeedCursor = { created_at: 1_699_000_000_000_000, video_id: 7 };

function makeItem(id: number, overrides: Partial<FeedItem> = {}): FeedItem {
  return {
    id,
    author_id: 7,
    username: 'megu',
    title: `第 ${id} 条视频`,
    play_url: `/videos/7/${id}.mp4`,
    cover_url: `/covers/${id}/a.jpg`,
    created_at: '2026-10-15T10:00:00Z',
    status: 1,
    play_count: 0,
    likes_count: 0,
    comment_count: 0,
    ...overrides,
  };
}

type Page = { items: FeedItem[]; nextCursor?: FeedCursor };

// 假的取数函数:按顺序吐出预设好的几页(Error 就是一次失败),并记下每一次被要走的游标。
// 页数只有测试准备的那些 —— 容器多翻一页就报错,而不是悄悄再给一页:
// "到底之后不许再拉"这类断言必须能被假实现打破,否则它是空的。
function makeLoad(pages: (Page | Error)[]) {
  return vi.fn(async (_cursor?: FeedCursor): Promise<Page> => {
    const page = pages.shift();
    if (!page) throw new Error('测试只准备了这么多页:load 被多调了一次');
    if (page instanceof Error) throw page;
    return page;
  });
}

// 手动可控的 Promise:用来把"请求还在飞"这个状态停住,好在那期间做断言。
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((res) => {
    resolve = res;
  });
  return { promise, resolve };
}

function renderList(load: ReturnType<typeof makeLoad>, emptyText: string = EMPTY_TEXT) {
  return render(
    <TestRouter>
      <FeedList<FeedCursor> load={load} emptyText={emptyText} />
    </TestRouter>,
  );
}

// 把已经排进微任务的 Promise 全部落地。假 load 是立即 resolve 的,一次空 act 只够冲一层 ——
// "空页 ⇒ 又跟着游标拉一页"这种链式翻页有两三层,所以连冲几次。
// 断言"没有多调"的用例必须先冲干净,否则数出来的次数没意义。
async function flush() {
  await act(async () => {});
  await act(async () => {});
  await act(async () => {});
}

describe('FeedList 只跟游标,不跟 items 长度', () => {
  it('第一页是空数组但带着游标 → 必须继续调 load(游标),不能判到底', async () => {
    // 第二页用手动控制的 Promise:第一页(空页)落地之后、第二页回来之前的那一小段
    // 正是"这一页没东西,但还没到底"的时刻 —— 断言要看的就是那一刻。
    const secondPage = deferred<Page>();
    const load = vi.fn((cursor?: FeedCursor) =>
      cursor === undefined
        ? // 第一页:一条都没有,但游标还在 —— 后端说"游标之后还有内容"
          Promise.resolve({ items: [], nextCursor: CURSOR_A })
        : secondPage.promise,
    );
    renderList(load);
    // 列表是空的 ⇒ 哨兵就在首屏里(用户的"触底"信号此刻已经成立)
    await atTail();
    await flush();

    // 关键断言:空页 != 到底 —— 必须带着游标再要一页
    expect(load).toHaveBeenCalledTimes(2);
    expect(load.mock.calls[1][0]).toEqual(CURSOR_A);
    // 而且这一刻不许把"空态 / 到底"摆出来(那正是"看长度判到底"的实现会做的事),
    // 屏幕上该说的是"加载中"。
    expect(screen.queryByText(EMPTY_TEXT)).not.toBeInTheDocument();
    expect(screen.queryByText(END_TEXT)).not.toBeInTheDocument();
    expect(screen.getByText(LOADING_TEXT)).toBeInTheDocument();

    // 第二页的内容确实上屏了
    await act(async () => {
      secondPage.resolve({ items: [makeItem(1)], nextCursor: undefined });
    });
    expect(screen.getByTestId('video-panel')).toBeInTheDocument();
  });

  it('空页落地、而哨兵还没进视口的那一帧:"加载中…",不是空态,也不是一片黑', async () => {
    const load = makeLoad([
      { items: [], nextCursor: CURSOR_A },
      { items: [makeItem(1)], nextCursor: undefined },
    ]);
    renderList(load);
    await flush();

    // 没触底就不自作主张地往下拉(下一页是"用户碰到尾部"这个信号驱动的)
    expect(load).toHaveBeenCalledTimes(1);
    // 但此刻屏幕上什么都没有,又不能摆"还没有视频"(那是在撒谎:后端说游标之后还有内容),
    // 也不许留一片黑 —— 该说的是"加载中…"。
    expect(screen.getByText(LOADING_TEXT)).toBeInTheDocument();
    expect(screen.queryByText(EMPTY_TEXT)).not.toBeInTheDocument();
    expect(screen.queryByText(END_TEXT)).not.toBeInTheDocument();

    await atTail();
    await flush();

    expect(load).toHaveBeenCalledTimes(2);
    expect(load.mock.calls[1][0]).toEqual(CURSOR_A);
    expect(screen.getByTestId('video-panel')).toBeInTheDocument();
  });

  it('滑到底(哨兵进视口)才拉下一页,带的是上一页给的游标', async () => {
    const load = makeLoad([
      { items: [makeItem(1)], nextCursor: CURSOR_B },
      { items: [makeItem(2)], nextCursor: undefined },
    ]);
    renderList(load);
    await flush();

    // 第一页只有 1 条(远短于一页的 limit),但游标在 ⇒ 不许当到底
    expect(load).toHaveBeenCalledTimes(1);
    expect(screen.queryByText(END_TEXT)).not.toBeInTheDocument();

    await atTail();
    await flush();

    expect(load).toHaveBeenCalledTimes(2);
    expect(load.mock.calls[1][0]).toEqual(CURSOR_B);
    expect(screen.getAllByTestId('video-panel')).toHaveLength(2);
  });

  it('游标缺失才算到底:渲染"没有更多了",并且之后一次都不再调 load', async () => {
    const load = makeLoad([{ items: [makeItem(1)], nextCursor: undefined }]);
    renderList(load);
    await flush();

    // 到底即渲染 —— 不需要先滑动去"证明"这件事
    expect(screen.getByText(END_TEXT)).toBeInTheDocument();
    expect(load).toHaveBeenCalledTimes(1);

    // 到底之后反复进出视口也不许再发(游标没了,再发只会把同一页要回来)
    await atTail();
    await notAtTail();
    await atTail();
    await flush();

    expect(load).toHaveBeenCalledTimes(1);
  });
});

describe('FeedList 的四种状态', () => {
  it('加载中:第一页还没回来时只有"加载中…",不提前摆出空态或到底', async () => {
    const firstPage = deferred<Page>();
    const load = vi.fn((_cursor?: FeedCursor) => firstPage.promise);
    renderList(load);

    expect(screen.getByText(LOADING_TEXT)).toBeInTheDocument();
    expect(load).toHaveBeenCalledTimes(1);
    // 还没有内容,也没有结论 —— 这时摆空态/到底都是在撒谎
    expect(screen.queryByText(EMPTY_TEXT)).not.toBeInTheDocument();
    expect(screen.queryByText(END_TEXT)).not.toBeInTheDocument();

    await act(async () => {
      firstPage.resolve({ items: [makeItem(1)], nextCursor: undefined });
    });

    expect(screen.queryByText(LOADING_TEXT)).not.toBeInTheDocument();
    expect(screen.getByTestId('video-panel')).toBeInTheDocument();
  });

  it('空:第一页既没有内容也没有下一页 → emptyText(不是"没有更多了")', async () => {
    const load = makeLoad([{ items: [], nextCursor: undefined }]);
    renderList(load);
    await flush();

    expect(screen.getByText(EMPTY_TEXT)).toBeInTheDocument();
    expect(screen.queryByText(END_TEXT)).not.toBeInTheDocument();
  });

  it('错误可重试:失败给重试入口,点它用同一个游标重来一页', async () => {
    const load = makeLoad([new Error('boom'), { items: [makeItem(1)], nextCursor: undefined }]);
    renderList(load);
    await flush();

    const retry = screen.getByRole('button', { name: ERROR_TEXT });
    expect(load).toHaveBeenCalledTimes(1);

    await userEvent.click(retry);
    await flush();

    // 重试打的是同一页(失败不许把游标推走,否则整页会被跳过)
    expect(load).toHaveBeenCalledTimes(2);
    expect(load.mock.calls[1][0]).toBeUndefined();
    expect(screen.getByTestId('video-panel')).toBeInTheDocument();
    expect(screen.queryByText(ERROR_TEXT)).not.toBeInTheDocument();
  });

  it('失败之后不会自动重发:必须由用户点重试(自动重发会打成重试风暴)', async () => {
    const load = makeLoad([new Error('boom')]);
    renderList(load);
    await flush();

    await atTail();
    await notAtTail();
    await atTail();
    await flush();

    expect(load).toHaveBeenCalledTimes(1);
    expect(screen.getByRole('button', { name: ERROR_TEXT })).toBeInTheDocument();
  });

  it('已经有视频在屏上时失败:视频留着,提示退到底部一小条(不整屏盖住画面)', async () => {
    const load = makeLoad([
      { items: [makeItem(1)], nextCursor: CURSOR_A },
      new Error('boom'),
    ]);
    renderList(load);
    await flush();

    await atTail();
    await flush();

    // 已经看到的那一屏不该因为下一页失败就消失
    expect(screen.getByTestId('video-panel')).toBeInTheDocument();
    const status = screen.getByTestId('feed-list-status');
    expect(screen.getByRole('button', { name: ERROR_TEXT })).toBeInTheDocument();
    // 提示挂在滚动容器**外面**:它钉在视口上,不随内容滚走(snap 吸附的容器里,
    // 放进流里的提示永远停不住)
    expect(screen.getByTestId('feed-list')).not.toContainElement(status);
  });
});

describe('FeedList 的并发保护', () => {
  it('StrictMode 把挂载 effect 跑两遍(开发态就是这样)时,首屏也只拉一次', async () => {
    // 应用是真的套着 <React.StrictMode> 跑的(main.tsx),而它会在开发态把挂载 effect
    // 跑两遍。两次 setup 之间 loading 这个 state 还没更新 —— 所以挡住第二次的只能是
    // 那把 ref 闸门(这一条就是它的可证伪用例)。
    const load = makeLoad([{ items: [makeItem(1)], nextCursor: undefined }]);
    render(
      <StrictMode>
        <TestRouter>
          <FeedList<FeedCursor> load={load} emptyText={EMPTY_TEXT} />
        </TestRouter>
      </StrictMode>,
    );
    await flush();

    expect(load).toHaveBeenCalledTimes(1);
    // 首屏那条也不许被追加成两条(两次响应都把同一页拼进去)
    expect(screen.getAllByTestId('video-panel')).toHaveLength(1);
  });

  it('第一页还在飞时哨兵就进了视口 → 只发一次请求', async () => {
    const firstPage = deferred<Page>();
    const load = vi.fn((_cursor?: FeedCursor) => firstPage.promise);
    renderList(load);

    await atTail();
    await flush();

    expect(load).toHaveBeenCalledTimes(1);

    await act(async () => {
      firstPage.resolve({ items: [makeItem(1)], nextCursor: undefined });
    });

    // 落地之后也不许多发一次(游标缺失 = 到底)
    expect(load).toHaveBeenCalledTimes(1);
  });

  it('下一页还在飞时反复进出视口 → 只发一次请求', async () => {
    const secondPage = deferred<Page>();
    const load = vi.fn((cursor?: FeedCursor) =>
      cursor === undefined
        ? Promise.resolve({ items: [makeItem(1)], nextCursor: CURSOR_A })
        : secondPage.promise,
    );
    renderList(load);
    await flush();

    await atTail();
    await flush();
    expect(load).toHaveBeenCalledTimes(2);
    // 在飞期间:提示是"加载中…",不是到底
    expect(screen.getByText(LOADING_TEXT)).toBeInTheDocument();

    await notAtTail();
    await atTail();
    await flush();

    expect(load).toHaveBeenCalledTimes(2);

    await act(async () => {
      secondPage.resolve({ items: [makeItem(2)], nextCursor: undefined });
    });

    expect(screen.getAllByTestId('video-panel')).toHaveLength(2);
  });
});

describe('FeedList 作为三流的组合点', () => {
  it('首屏 items 非空 → 每条一个 VideoPanel,ActionRail 从右侧槽位进去且接的是同一条视频', async () => {
    const load = makeLoad([
      {
        items: [
          makeItem(1, { author_id: 7, username: 'megu' }),
          makeItem(2, { author_id: 8, username: 'kagu' }),
        ],
        nextCursor: undefined,
      },
    ]);
    renderList(load);
    await flush();

    const panels = screen.getAllByTestId('video-panel');
    expect(panels).toHaveLength(2);

    const slots = screen.getAllByTestId('video-panel-actions');
    const rails = screen.getAllByTestId('action-rail');
    expect(slots).toHaveLength(2);
    expect(rails).toHaveLength(2);
    // 每条视频一个右侧栏,而且落在它自己那一屏的槽位里(不是全堆在最后一条上)
    expect(slots[0]).toContainElement(rails[0]);
    expect(slots[1]).toContainElement(rails[1]);

    // 右侧栏拿到的必须是**这一屏**的那条视频:作者 id 对得上,顺序也对得上
    expect(panels[0]).toContainElement(screen.getByRole('link', { name: '查看 megu 的个人主页' }));
    expect(screen.getByRole('link', { name: '查看 megu 的个人主页' })).toHaveAttribute(
      'href',
      '/users/7',
    );
    expect(screen.getByRole('link', { name: '查看 kagu 的个人主页' })).toHaveAttribute(
      'href',
      '/users/8',
    );
  });

  it('滚动容器:一屏一个 + y 吸附(吸附写在每一项上,因为 VideoPanel 不接受 style)', async () => {
    const load = makeLoad([
      { items: [makeItem(1), makeItem(2)], nextCursor: undefined },
    ]);
    renderList(load);
    await flush();

    const scroller = screen.getByTestId('feed-list');
    expect(scroller.style.overflowY).toBe('auto');
    // 滚动行为留在 overflowY 上(滚轮 / 触摸 / 键盘 / 吸附),但**滚动条本身**由这条类名的
    // CSS 藏掉:classic 滚动条会在滚动区里侧吃掉约 15px,视频右缘之外于是露出一条底色,
    // "满屏出血"只是近似。伪元素(::-webkit-scrollbar)inline 表达不了,所以规则在 feed.css 里;
    // 这一条钉的是"钩子还在" —— 类名一掉,那份 CSS 就成了死代码。
    // (jsdom 不跑样式表,真正的滚动条行为只能在真机上看,见 task-11 走查清单。)
    expect(scroller).toHaveClass('feed-scroller');
    // 满屏流要的是"一定对齐",所以是 mandatory 而不是 proximity
    expect(scroller.style.scrollSnapType).toBe('y mandatory');

    const items = screen.getAllByTestId('feed-list-item');
    expect(items).toHaveLength(2);
    expect(items[0].style.scrollSnapAlign).toBe('start');
    expect(items[1].style.scrollSnapAlign).toBe('start');

    // 自己撑满一屏,不依赖父容器高度(父容器 auto 高时 height:100% 会算成 0)
    const root = screen.getByTestId('feed-list-root');
    expect(root.style.height).toBe('100vh');
    expect(root.style.maxHeight).toBe('100dvh');
  });

  it('哨兵的阈值是 0:压着一两个像素就算"碰到尾部了"(0.6 会让它永远等不到)', async () => {
    const load = makeLoad([{ items: [makeItem(1)], nextCursor: CURSOR_A }]);
    renderList(load);

    expect(sentinelObserver().options?.threshold).toBe(0);

    // 把首屏那次请求就地落地:不让它在用例结束后才 setState(那会打出 act 警告,
    // 而且警告会算到"下一个"用例头上,很难定位)。
    await flush();
  });
});

// 状态浮层是钉在视口上的(见 FeedList 里"为什么挂在滚动容器外面"),所以它按**自己的盒子**居中;
// 而左侧那条导航是 fixed 浮层,盖在视频上、占掉视口最左 200px。窄屏(375px)下居中就落在了导航底下 ——
// 那行字里正好是重试按钮,只露出约 38px 可点。修法是让**浮层自己**让开,
// 而不是把整页 / 视频缩窄(spec §7/§12 的满屏出血是用户明确选的,缩窄已经被否掉一次)。
// jsdom 没有布局,这两条钉的是**声明出来的几何**(值对了、接线对了),像素要真机看。
describe('FeedList 的状态浮层躲开左侧那条浮层导航', () => {
  it('整屏居中的浮层(加载中 / 空 / 首屏失败)从侧栏右缘起画', () => {
    const firstPage = deferred<Page>();
    const load = vi.fn((_cursor?: FeedCursor) => firstPage.promise);
    renderList(load);

    const status = screen.getByTestId('feed-list-status');
    // 200 = SIDEBAR_WIDTH(MainLayout 那条 fixed 导航的宽度)。浮层从 (0,0) 起的话,
    // 375px 宽下居中的那行字正好压在导航底下 —— 可点的只剩一小截。
    expect(status.style.left).toBe('200px');
    expect(status.style.right).toBe('0px');

    return act(async () => {
      firstPage.resolve({ items: [], nextCursor: undefined });
    });
  });

  it('已经有视频时失败那一小条也让位(同一个居中,同一种遮挡)', async () => {
    const load = makeLoad([
      { items: [makeItem(1)], nextCursor: CURSOR_A },
      new Error('boom'),
    ]);
    renderList(load);
    await flush();

    await atTail();
    await flush();

    const status = screen.getByTestId('feed-list-status');
    expect(status.style.left).toBe('200px');
    expect(status.style.right).toBe('0px');
    // 居中改成"在导航右边那一条里居中":原来的 translateX(-50%) 是按整宽居中,会钻到导航底下
    expect(status.style.transform).toBe('');
  });
});

// "没有更多了"的时机。
//
// 浮层钉在视口上 ⇒ 这行字一出现就压在**当前这一屏**上。最后一页完全可能有不止一条:
// 一页 5 条、游标恰好用尽时,你还在看第 1 条,它就已经压在画面上了 —— 说的是"后面没有了",
// 却出在你还没看的内容上。所以它现在受一道门:「用户到过尾部」之后才摆出来。
//
// 门有两个信号,OR 起来、而且一旦为真就不再回退(latch,浮层不许闪):
//   ① 哨兵进过视口 —— 与"触底加载"同一个信号(容器里"到达尾部"只有一个概念);
//   ② 滚动位置已经贴到底 —— 哨兵是列表末尾之后的 1px,而每一屏都正好一屏高,
//      满屏吸附会把滚动位置拉回到最后一屏的顶边,哨兵于是卡在滚动区下沿**之外**;
//      单靠 ①,这行字可能永远不出现(单条一页时更是连 1px 都拖不动)。
//
// —— 替身 3:滚动几何(按 DOM 里的屏数实时算) ——
// jsdom 没有布局:scrollTop / clientHeight / scrollHeight 一律是 0,上面那个判定就恒为真
// (0 + 0 >= 0 - 2),被测的会是巧合而不是逻辑。所以把这三条值装成"最接近真实布局"的样子:
// 容器正好一屏高,每条视频也正好一屏,列表末尾那个 1px 哨兵也算进去 —— 屏数直接从 DOM 里数。
// 这样连"首屏还没有内容时(滚动区里只有那个哨兵)不能算到底"都能被摆出来。
// 诚实边界:它证明的是**判定与接线**(读哪三条值、什么时候读、latch 会不会回退、空列表算不算到底),
// 不是"真实布局下到底对不对" —— 那要真机(task-11 走查清单第 2 条)。
const SCREEN_PX = 720;
const GEOMETRY_KEYS = ['scrollTop', 'clientHeight', 'scrollHeight'] as const;

let scrollTopStub = 0;

function stubListGeometry() {
  scrollTopStub = 0;
  Object.defineProperty(HTMLElement.prototype, 'clientHeight', {
    configurable: true,
    get: () => SCREEN_PX,
  });
  Object.defineProperty(HTMLElement.prototype, 'scrollHeight', {
    configurable: true,
    // 一条视频一屏,末尾再加那个 1px 哨兵 —— 从 DOM 里数,和真实布局同一套算法
    get: () => document.querySelectorAll('[data-testid="feed-list-item"]').length * SCREEN_PX + 1,
  });
  Object.defineProperty(HTMLElement.prototype, 'scrollTop', {
    configurable: true,
    get: () => scrollTopStub,
    set: (value: number) => {
      scrollTopStub = value;
    },
  });
}

// jsdom 把这三个属性定义在 Element.prototype 上(实测),所以在 HTMLElement.prototype 上加一层
// 自己的存取器就是"遮住它";删掉这层,继承下来的那份立刻回来(也是实测过的)。
function unstubGeometry() {
  for (const key of GEOMETRY_KEYS) {
    delete (HTMLElement.prototype as unknown as Record<string, unknown>)[key];
  }
}

const scroller = () => screen.getByTestId('feed-list');

describe('到底那行字只在用户到过尾部之后才摆出来', () => {
  beforeEach(() => {
    stubListGeometry();
  });

  afterEach(() => {
    unstubGeometry();
  });

  it('游标已经没了、但用户还在上面:先不摆这行字', async () => {
    const load = makeLoad([{ items: [makeItem(1), makeItem(2)], nextCursor: undefined }]);
    renderList(load);
    await flush();

    // "到底"这件事已经成立(游标缺失),但几何说还在最上面(两屏内容,位置 0)⇒ 不该多出一行字。
    // 这一条同时守着"首屏还没有内容时**不算**到底":那一刻滚动区里只有哨兵,
    // 任何容差下都"已经在底部" —— 若在挂载时就判,这道门等于不存在。
    expect(screen.queryByText(END_TEXT)).not.toBeInTheDocument();
    expect(screen.queryByTestId('feed-list-status')).not.toBeInTheDocument();
    // 有视频在屏上,该有的东西一个不少(这道门只影响那行字)
    expect(screen.getAllByTestId('video-panel')).toHaveLength(2);
  });

  it('哨兵进过视口 ⇒ 摆出来;之后滑回去也不收回(latch,浮层不许闪)', async () => {
    const load = makeLoad([{ items: [makeItem(1), makeItem(2)], nextCursor: undefined }]);
    renderList(load);
    await flush();

    await atTail();
    expect(screen.getByText(END_TEXT)).toBeInTheDocument();

    await notAtTail();
    expect(screen.getByText(END_TEXT)).toBeInTheDocument();
  });

  it('滚动位置已经贴到底 ⇒ 也摆出来(哨兵卡在滚动区下沿之外的兜底)', async () => {
    const load = makeLoad([{ items: [makeItem(1), makeItem(2)], nextCursor: undefined }]);
    renderList(load);
    await flush();
    expect(screen.queryByText(END_TEXT)).not.toBeInTheDocument();

    // 摆出"已经滑到最后一屏"的滚动位置(2 屏内容 ⇒ 第二屏的顶边 = 1 × 720),并派发一次 scroll
    await act(async () => {
      scroller().scrollTop = SCREEN_PX;
      fireEvent.scroll(scroller());
    });

    expect(screen.getByText(END_TEXT)).toBeInTheDocument();
  });

  it('滚回顶部 ⇒ 收起这行字(它钉在视口上,不该继续压着已经看过的内容)', async () => {
    const load = makeLoad([{ items: [makeItem(1), makeItem(2)], nextCursor: undefined }]);
    renderList(load);
    await flush();

    await act(async () => {
      scroller().scrollTop = SCREEN_PX;
      fireEvent.scroll(scroller());
    });
    expect(screen.getByText(END_TEXT)).toBeInTheDocument();

    // 往回滚:浮层钉在视口上,用户回到第 1 条时这行字还挂在画面中央 —— 说的是"后面没有了",
    // 却站在一条他正在看的内容上。所以"贴到底"这个信号必须是**双向**的(离开底部就收回)。
    await act(async () => {
      scroller().scrollTop = 0;
      fireEvent.scroll(scroller());
    });
    expect(screen.queryByText(END_TEXT)).not.toBeInTheDocument();

    // 再滑回去还得重新摆出来(收回的不是这道门本身,只是那一刻的状态)
    await act(async () => {
      scroller().scrollTop = SCREEN_PX;
      fireEvent.scroll(scroller());
    });
    expect(screen.getByText(END_TEXT)).toBeInTheDocument();
  });

  it('到底但一条都没有:空态文案不受这道门影响(屏幕上什么都没有,必须有话说)', async () => {
    const load = makeLoad([{ items: [], nextCursor: undefined }]);
    renderList(load);
    await flush();

    expect(screen.getByText(EMPTY_TEXT)).toBeInTheDocument();
    expect(screen.queryByText(END_TEXT)).not.toBeInTheDocument();
  });
});

// 没有 IntersectionObserver 的环境(老浏览器 / 非浏览器):useInView 会一直回"不可见",
// 哨兵于是永远不会触发 —— "触底加载"就没有任何自动入口了。首屏若恰好一条都没有、却还带着游标
// (合法状态:本页候选被截断 / 窗口内的条目已失效),屏幕上就是一句永远的"加载中…",没有出口。
// 所以那个状态必须有一个**手动**出口,而且它得是个真控件(键盘 / 读屏可达)。
describe('没有 IntersectionObserver 的环境:触底加载有手动出口', () => {
  // jsdom 本来就没有 IntersectionObserver;上面的 beforeEach 为了别的用例装了替身,
  // 这里把它摘掉,退回那个环境(每个用例的 beforeEach 都会重新装上,不必在 afterEach 里还原)。
  beforeEach(() => {
    delete (globalThis as { IntersectionObserver?: unknown }).IntersectionObserver;
  });

  it('首屏空、但游标还在:不是永远的"加载中…",而是一个点得到的出口', async () => {
    const load = makeLoad([
      { items: [], nextCursor: CURSOR_A },
      { items: [makeItem(1)], nextCursor: undefined },
    ]);
    renderList(load);
    await flush();

    expect(screen.queryByText(LOADING_TEXT)).not.toBeInTheDocument();
    const more = screen.getByRole('button', { name: MORE_TEXT });
    expect(load).toHaveBeenCalledTimes(1);

    await userEvent.click(more);
    await flush();

    // 出口是真的:带着上一页给的游标把下一页要回来
    expect(load).toHaveBeenCalledTimes(2);
    expect(load.mock.calls[1][0]).toEqual(CURSOR_A);
    expect(screen.getByTestId('video-panel')).toBeInTheDocument();
  });

  it('已经有内容、游标还在:底部那一小条是"加载更多",点了继续往下拉', async () => {
    const load = makeLoad([
      { items: [makeItem(1)], nextCursor: CURSOR_A },
      { items: [makeItem(2)], nextCursor: undefined },
    ]);
    renderList(load);
    await flush();

    await userEvent.click(screen.getByRole('button', { name: MORE_TEXT }));
    await flush();

    expect(load).toHaveBeenCalledTimes(2);
    expect(load.mock.calls[1][0]).toEqual(CURSOR_A);
    expect(screen.getAllByTestId('video-panel')).toHaveLength(2);
    // 游标用尽之后这个手动出口就该消失 —— 到底了,没有下一页可拉
    expect(screen.queryByRole('button', { name: MORE_TEXT })).not.toBeInTheDocument();
    expect(screen.getByText(END_TEXT)).toBeInTheDocument();
  });
});

describe('到底那行字不会被"内容到位之前的哨兵信号"提前打开', () => {
  beforeEach(() => {
    stubListGeometry();
  });

  afterEach(() => {
    unstubGeometry();
  });

  it('首屏是"空页 + 有游标"时,哨兵在内容到位前发出的可见信号不算数', async () => {
    // 首帧里 items 还是空的,而哨兵是滚动区里唯一的孩子 —— 它就在视口里。
    // 浏览器那次初始 IO 回调(~1 帧,React 里看不见)会立刻报 isIntersecting。
    // 若信号 ① 不判"有没有内容",这道门在首屏就被打开:等第二页也到底时,用户其实什么都没看到,
    // 屏幕上却已经摆着"没有更多了" —— 正是这道门要挡的那种情形。
    const load = makeLoad([
      { items: [], nextCursor: CURSOR_B },
      { items: [makeItem(1), makeItem(2), makeItem(3)], nextCursor: undefined },
    ]);

    renderList(load);
    // 首屏还没有内容时,哨兵"进过视口"这一下(它同时也是"拉下一页"的信号)
    await atTail();
    await flush();

    expect(load).toHaveBeenCalledTimes(2);
    // 第二页有内容、游标也没了(确实到底),但用户从没滑到底过 → 先不摆这行字
    expect(screen.queryByText(END_TEXT)).not.toBeInTheDocument();
  });
});
