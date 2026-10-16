import { act, fireEvent, render, screen } from '@testing-library/react';
import type { ReactNode } from 'react';
import { palette } from '../../theme/tokens';
import type { FeedItem } from '../../types/feed';
import VideoPanel from './VideoPanel';

// 这个文件要证明的核心只有一件事:**播放权由「在不在视口里」决定**。
// 首页会一次性挂上 N 个面板(每屏一个),所以「挂载了就播」是错的:必须有人告诉它
// 现在是不是它那一屏。jsdom 既没有 IntersectionObserver 也没有媒体播放实现,
// 两者都得装成可遥控 / 可断言的替身,否则测的只会是"props 传进去了"这种纸面结论。

// —— 替身 1:IntersectionObserver ——
// 与 hooks/useInView.test.ts 同一套写法:把回调和被观察元素交到测试手上,
// 测试就能像浏览器一样主动发一次「滚进 / 滚出视口」。
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

  static latest(): FakeIntersectionObserver {
    const observer = FakeIntersectionObserver.instances.at(-1);
    if (!observer) throw new Error('没有创建 IntersectionObserver');
    return observer;
  }
}

// —— 替身 2:媒体播放 ——
// jsdom 里 HTMLMediaElement.prototype.play / pause 只是往虚拟控制台扔一条
// "not implemented" 并把 undefined 当返回值。换成替身之后:
// play 返回 Promise(和浏览器一致 —— 组件里接住了它的 catch),pause 只记录被调用。
// 断言「play / pause 真的被调到」,而不是"某个属性传对了"。
let media: { play: ReturnType<typeof vi.fn>; pause: ReturnType<typeof vi.fn> };

const realIntersectionObserver = globalThis.IntersectionObserver;
const realPlay = HTMLMediaElement.prototype.play;
const realPause = HTMLMediaElement.prototype.pause;

beforeEach(() => {
  FakeIntersectionObserver.instances = [];
  globalThis.IntersectionObserver = FakeIntersectionObserver as unknown as typeof IntersectionObserver;

  media = { play: vi.fn().mockResolvedValue(undefined), pause: vi.fn() };
  HTMLMediaElement.prototype.play = media.play as unknown as typeof realPlay;
  HTMLMediaElement.prototype.pause = media.pause as unknown as typeof realPause;
});

afterEach(() => {
  globalThis.IntersectionObserver = realIntersectionObserver;
  HTMLMediaElement.prototype.play = realPlay;
  HTMLMediaElement.prototype.pause = realPause;
});

const ITEM: FeedItem = {
  id: 1,
  author_id: 7,
  username: 'megu',
  title: '第一条视频',
  description: '视频的描述文案',
  play_url: '/videos/7/a.mp4',
  cover_url: '/covers/1/a.jpg',
  created_at: '2026-10-15T10:00:00Z',
  status: 1,
  play_count: 10,
  likes_count: 2,
  comment_count: 3,
};

function renderPanel(children?: ReactNode) {
  const view = render(<VideoPanel video={ITEM}>{children}</VideoPanel>);
  return {
    ...view,
    video: view.container.querySelector('video') as HTMLVideoElement,
    observer: FakeIntersectionObserver.latest(),
  };
}

// pause 的调用次数里混着「挂载时那次」,所以比较增量而不是绝对次数 ——
// 这样断言的是"这次滚动造成了什么",不是实现的调用总数。
function pauseCalls() {
  return media.pause.mock.calls.length;
}

describe('VideoPanel 的播放权只属于视口里那一屏', () => {
  it('挂载时不在视口里:不调 play,并且主动 pause', () => {
    const { video } = renderPanel();

    // 一页 N 个面板同时挂载,任何一个都不该自己播起来
    expect(media.play).not.toHaveBeenCalled();
    expect(media.pause).toHaveBeenCalled();
    expect(video.muted).toBe(true);
  });

  it('滚进视口才 play —— 播放由可见性驱动', () => {
    const { observer } = renderPanel();
    expect(media.play).not.toHaveBeenCalled();

    act(() => observer.emit(true));

    expect(media.play).toHaveBeenCalledTimes(1);
  });

  it('滚出视口就 pause,并且不会又播起来', () => {
    const { observer } = renderPanel();

    act(() => observer.emit(true));
    expect(media.play).toHaveBeenCalledTimes(1);

    const before = pauseCalls();
    act(() => observer.emit(false));

    expect(pauseCalls()).toBe(before + 1);
    expect(media.play).toHaveBeenCalledTimes(1);
  });

  it('可见 → 不可见 → 再可见:来回切换都由可见性驱动', () => {
    const { observer } = renderPanel();

    act(() => observer.emit(true));
    act(() => observer.emit(false));
    act(() => observer.emit(true));

    expect(media.play).toHaveBeenCalledTimes(2);
  });
});

describe('VideoPanel 的媒体属性', () => {
  it('不带 autoPlay 属性:自播靠可见性,不靠属性(有属性就会 N 个同时播)', () => {
    const { video } = renderPanel();

    expect(video).not.toHaveAttribute('autoplay');
    expect(video).not.toHaveAttribute('autoPlay');
  });

  it('preload 只要 metadata:不预载整份文件,滑走的面板不该吃带宽', () => {
    const { video } = renderPanel();

    expect(video.getAttribute('preload')).toBe('metadata');
  });

  it('循环、行内播放、默认静音,封面与播放地址直接用站内相对路径', () => {
    const { video } = renderPanel();

    expect(video).toHaveAttribute('loop');
    // iOS 上不加 playsinline 会强制全屏接管,iPhone 上就是"一点就跳出去"
    expect(video).toHaveAttribute('playsinline');
    // 默认静音:带声音自动播放会被浏览器直接拦掉
    expect(video.muted).toBe(true);
    expect(video.getAttribute('poster')).toBe(ITEM.cover_url);
    expect(video.getAttribute('src')).toBe(ITEM.play_url);
  });

  it('点一下面板切换静音:静音 → 有声 → 静音', () => {
    const { video } = renderPanel();

    fireEvent.click(screen.getByTestId('video-panel'));
    expect(video.muted).toBe(false);

    fireEvent.click(screen.getByTestId('video-panel'));
    expect(video.muted).toBe(true);
  });
});

describe('VideoPanel 的画面上浮层', () => {
  it('左下角是作者与标题,description 有才渲染', () => {
    const { unmount } = renderPanel();
    expect(screen.getByText('@megu')).toBeInTheDocument();
    expect(screen.getByText('第一条视频')).toBeInTheDocument();
    expect(screen.getByText('视频的描述文案')).toBeInTheDocument();
    unmount();

    render(<VideoPanel video={{ ...ITEM, description: undefined }} />);
    expect(screen.queryByText('视频的描述文案')).not.toBeInTheDocument();
  });

  it('浮在画面上的文字压在渐变遮罩上,颜色取深色 token,保证对比度', () => {
    renderPanel();

    // 遮罩:底边起从沉浸底渐变到透明,铺满整个底部(右侧操作栏也在这条带上)
    const scrim = screen.getByTestId('video-panel-scrim');
    expect(scrim.style.backgroundImage).toContain('linear-gradient');
    expect(scrim.style.backgroundImage).toContain(palette.background);
    expect(scrim.style.left).toBe('0px');
    expect(scrim.style.right).toBe('0px');
    expect(scrim.style.bottom).toBe('0px');

    // 文字:主文字 / 次文字都必须取 palette 那一份(不是自己抄一遍 hex)——
    // 断的是 **palette 里的那个值**,所以改 palette 会同时改到组件与这条断言(同源)。
    // palette 本身等于 spec §13 那张表的值,由 theme/tokens.test.tsx 钉住。
    expect(screen.getByText('@megu')).toHaveStyle({ color: palette.textPrimary });
    expect(screen.getByText('第一条视频')).toHaveStyle({ color: palette.textPrimary });
    expect(screen.getByText('视频的描述文案')).toHaveStyle({ color: palette.textSecondary });
  });

  it('左下角的文字避让左侧浮层导航:左内边距按侧栏宽度留出', () => {
    renderPanel();

    // 侧栏是浮在视频上的(满屏出血,spec §7/§12),不避让的话作者名会被压在导航底下。
    // 200(侧栏) + 16(spacing.md) = 216 —— 与右侧避让操作栏的 88px 对称。
    expect(screen.getByTestId('video-panel-info').style.paddingLeft).toBe('216px');
  });

  it('children 渲染在右侧浮层槽位(给 Task 8 的 ActionRail 留的位置)', () => {
    const { container } = renderPanel(<button type="button">点赞</button>);

    const slot = screen.getByTestId('video-panel-actions');
    expect(slot).toContainElement(screen.getByRole('button', { name: '点赞' }));
    // 槽位在面板内部 —— 浮在视频上,而不是挤在画面外
    expect(container.querySelector('[data-testid="video-panel"]')).toContainElement(slot);
  });

  it('点浮层里的按钮不会顺带把声音开关切了(不靠"记得给按钮 stopPropagation")', () => {
    const { video } = renderPanel(<button type="button">点赞</button>);

    fireEvent.click(screen.getByRole('button', { name: '点赞' }));

    expect(video.muted).toBe(true);
  });
});

describe('VideoPanel 的可达性 / 视口适配 / 失败可见性(复审补充)', () => {
  it('声音开关是语义化按钮:键盘与读屏用户唯一可达的入口', () => {
    const { video } = renderPanel();

    // 整屏 div 上的 onClick 对键盘用户不可达 —— 必须有一个真控件
    const toggle = screen.getByRole('button', { name: '视频声音' });
    // aria-pressed 表达"这个开关现在是不是开着的" → 默认静音 = 声音没开 = false
    expect(toggle).toHaveAttribute('aria-pressed', 'false');

    fireEvent.click(toggle);

    expect(video.muted).toBe(false);
    expect(screen.getByRole('button', { name: '视频声音' })).toHaveAttribute('aria-pressed', 'true');
  });

  it('被自动播放策略拦下时保持安静,真播放失败要现形', async () => {
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { observer } = renderPanel();

    // NotAllowedError:用户还没交互,浏览器拦下自动播放 —— 预期内,不该刷控制台
    media.play.mockRejectedValueOnce(new DOMException('blocked', 'NotAllowedError'));
    await act(async () => {
      observer.emit(true);
    });
    expect(errorSpy).not.toHaveBeenCalled();

    // NotSupportedError:这一屏根本播不了,用户只会看到一张不动的封面 —— 必须报出来
    media.play.mockRejectedValueOnce(new DOMException('bad source', 'NotSupportedError'));
    await act(async () => {
      observer.emit(false);
    });
    await act(async () => {
      observer.emit(true);
    });
    expect(errorSpy).toHaveBeenCalled();

    errorSpy.mockRestore();
  });

  it('面板用动态视口高(dvh),遮罩高度带像素下限 —— 矮视口下文字仍落在暗段上', () => {
    renderPanel();

    // 100vh 是"大视口"高,移动端带地址栏时底部会被压到地址栏下;dvh 跟着地址栏伸缩
    expect(screen.getByTestId('video-panel').style.maxHeight).toBe('100dvh');

    // 纯百分比遮罩会在矮视口被压扁,文字滑到渐变很淡的一段上(实测 ≈2.9:1)
    expect(screen.getByTestId('video-panel-scrim').style.height).toMatch(/px/);
  });

  it('点画面重试播放时,真失败也要现形 —— 不是一律吞掉', async () => {
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { video } = renderPanel();

    // jsdom 没有媒体实现,"这一屏现在没在播"这个事实只能自己钉住 ——
    // toggleMute 只在 paused 时借这次点击重试 play()(它就是这条分支的唯一入口)。
    Object.defineProperty(video, 'paused', { configurable: true, value: true });
    media.play.mockRejectedValueOnce(new DOMException('bad source', 'NotSupportedError'));

    fireEvent.click(screen.getByTestId('video-panel'));
    await act(async () => {});

    // 这次重试的失败分类必须与"视口驱动的那次播放"**同一套**:NotAllowedError / AbortError 静默,
    // 其余现形。原来这里是个空 catch(() => {}) —— 与六行之下那条分类策略自相矛盾,
    // 真播不了的时候控制台里什么都没有。
    expect(errorSpy).toHaveBeenCalled();

    errorSpy.mockRestore();
  });
});
