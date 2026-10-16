import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { ReactNode } from 'react';
import { MemoryRouter, useLocation } from 'react-router-dom';
import { likeVideo, unlikeVideo } from '../../api/video';
import type { FeedItem } from '../../types/feed';
import type { LikeResp } from '../../types/video';
import ActionRail from './ActionRail';

// 这个文件要证明的是**行为**,不是"props 传进去了":
//   ① 点赞的初始态来自本条视频,点下去打的是与当前状态匹配的那个接口;
//   ② 渲染出来的计数跟的是**响应里的真值**,不是本地 ±1(并发下只有服务端知道真值);
//   ③ 评论格只有图标 + 数字,点它什么都不发生(后端没有评论模块);
//   ④ 点头像真的会走到作者主页(真路由跑一遍,而不是断言某个函数被调到)。
//
// api 一律 mock:这里不测 HTTP,axios 拦截器(弹错误 message)由 api 层自己负责,
// 组件只关心"拿到响应之后界面变成什么样"。
vi.mock('../../api/video', () => ({
  likeVideo: vi.fn(),
  unlikeVideo: vi.fn(),
}));

const likeVideoMock = vi.mocked(likeVideo);
const unlikeVideoMock = vi.mocked(unlikeVideo);

beforeEach(() => {
  likeVideoMock.mockReset();
  unlikeVideoMock.mockReset();
});

// 路由探针:把当前 pathname 画在 DOM 上。
// 为什么不 mock useNavigate 断言:那样验到的只是"组件调了某个函数",
// 而"点头像会到作者主页"这件事,只有让 react-router 真的处理一次点击才算验过 ——
// 链接的 href、路由匹配、URL 变化全都一起被覆盖。
function LocationProbe() {
  const location = useLocation();
  return <span data-testid="location">{location.pathname}</span>;
}

// react-router v6 对 v7 的两处行为变更会往 stderr 刷警告。这里提前打开 future flag:
// 测试输出保持干净,而且断言用的是 v7 的解析规则(不依赖旧的相对路径行为)。
function TestRouter({ children }: { children: ReactNode }) {
  return (
    <MemoryRouter future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
      {children}
    </MemoryRouter>
  );
}

const ITEM: FeedItem = {
  id: 42,
  author_id: 7,
  username: 'megu',
  avatar_url: '/avatars/7/a.jpg',
  title: '第一条视频',
  play_url: '/videos/7/a.mp4',
  cover_url: '/covers/42/a.jpg',
  created_at: '2026-10-15T10:00:00Z',
  status: 1,
  play_count: 10,
  likes_count: 3,
  comment_count: 8,
  is_like: false,
};

function renderRail(overrides: Partial<FeedItem> = {}) {
  const view = render(
    <TestRouter>
      <ActionRail video={{ ...ITEM, ...overrides }} />
      <LocationProbe />
    </TestRouter>,
  );
  return view;
}

// 点赞按钮的三个观察口:aria-pressed 看状态、testid 做交互、文本内容看计数。
// 计数只从渲染结果里读,不去碰组件内部状态 —— 否则测的是实现,不是界面。
// 计数用 /^N$/ 精确匹配而不是 toHaveTextContent('N'):后者是子串匹配(内容若是 '15' 也能过),
// 而"响应是 5"这条断言的全部价值就在于它排除掉别的数字。
const likeButton = () => screen.getByTestId('action-rail-like');

describe('ActionRail 的点赞接口接线', () => {
  it('初始态来自本条视频:is_like=false → 未按下,计数渲染 likes_count', () => {
    renderRail();

    expect(likeButton()).toHaveAttribute('aria-pressed', 'false');
    expect(likeButton()).toHaveTextContent(/^3$/);
  });

  it('未点赞时点击 → likeVideo(id),计数用响应里的真值(不是本地 +1)', async () => {
    // 本地是 3、后端回 5:一个"乐观 +1"的实现会渲染 4,所以这条断言能把它挑出来
    likeVideoMock.mockResolvedValue({ is_like: true, likes_count: 5 });
    renderRail();

    await userEvent.click(likeButton());

    expect(likeVideoMock).toHaveBeenCalledWith(ITEM.id);
    expect(unlikeVideoMock).not.toHaveBeenCalled();

    await waitFor(() => expect(likeButton()).toHaveAttribute('aria-pressed', 'true'));
    expect(likeButton()).toHaveTextContent(/^5$/);
  });

  it('已点赞时点击 → unlikeVideo(id),同样以响应纠正状态', async () => {
    unlikeVideoMock.mockResolvedValue({ is_like: false, likes_count: 2 });
    renderRail({ is_like: true, likes_count: 3 });

    expect(likeButton()).toHaveAttribute('aria-pressed', 'true');

    await userEvent.click(likeButton());

    expect(unlikeVideoMock).toHaveBeenCalledWith(ITEM.id);
    expect(likeVideoMock).not.toHaveBeenCalled();

    await waitFor(() => expect(likeButton()).toHaveAttribute('aria-pressed', 'false'));
    expect(likeButton()).toHaveTextContent(/^2$/);
  });

  it('请求还在飞的时候再点不会重复发请求(两次响应乱序会把计数带错)', async () => {
    let settle: (resp: LikeResp) => void = () => {};
    likeVideoMock.mockReturnValue(
      new Promise<LikeResp>((resolve) => {
        settle = resolve;
      }),
    );
    renderRail();

    await userEvent.click(likeButton());
    await userEvent.click(likeButton());

    expect(likeVideoMock).toHaveBeenCalledTimes(1);

    await act(async () => {
      settle({ is_like: true, likes_count: 4 });
    });

    expect(likeButton()).toHaveTextContent(/^4$/);
    expect(likeButton()).toHaveAttribute('aria-pressed', 'true');
  });

  it('点赞失败:状态不动、不弹面板 —— 错误提示归 axios 拦截器,组件不重复弹', async () => {
    likeVideoMock.mockRejectedValue(new Error('boom'));
    renderRail();

    await userEvent.click(likeButton());
    // 等 rejection 落地,确认它没有把状态改成"已点赞"
    await act(async () => {});

    expect(likeButton()).toHaveAttribute('aria-pressed', 'false');
    expect(likeButton()).toHaveTextContent(/^3$/);
    expect(screen.queryByRole('dialog')).toBeNull();
  });
});

describe('ActionRail 的评论格(后端没有评论模块)', () => {
  it('只有图标 + 数字,不是一个"点了没反应"的控件', () => {
    renderRail({ comment_count: 8 });

    // 读屏听到的是一个有名字的图像(「评论 8」),而不是一个孤零零的数字
    expect(screen.getByRole('img', { name: '评论 8' })).toBeInTheDocument();
    // 能聚焦却没有任何作用的按钮对键盘 / 读屏用户是陷阱,所以它不该是控件
    expect(screen.queryByRole('button', { name: /评论/ })).toBeNull();
    expect(screen.queryByRole('link', { name: /评论/ })).toBeNull();
  });

  it('点它什么都不发生:不弹面板、不跳转', async () => {
    renderRail({ comment_count: 8 });

    await userEvent.click(screen.getByRole('img', { name: '评论 8' }));

    expect(screen.queryByRole('dialog')).toBeNull();
    expect(screen.getByTestId('location')).toHaveTextContent('/');
    expect(screen.getByRole('img', { name: '评论 8' })).toBeInTheDocument();
  });
});

describe('ActionRail 的头像', () => {
  it('点头像进作者主页(/users/{author_id}),真走一遍路由', async () => {
    renderRail();

    const avatar = screen.getByRole('link', { name: '查看 megu 的个人主页' });
    await userEvent.click(avatar);

    await waitFor(() => expect(screen.getByTestId('location')).toHaveTextContent('/users/7'));
  });

  it('没有 avatar_url 时给占位头像,不渲染一张破图', () => {
    renderRail({ avatar_url: undefined });

    expect(screen.getByTestId('action-rail-avatar').querySelector('img')).toBeNull();
    // 头像框本身还在,作者主页的入口不能因为缺头像就消失
    expect(screen.getByRole('link', { name: '查看 megu 的个人主页' })).toBeInTheDocument();
  });
});

describe('ActionRail 的槽位与可读性', () => {
  it('四个槽位自上而下:头像 / 点赞 / 评论 / 关注槽位', () => {
    renderRail();

    const slots = Array.from(screen.getByTestId('action-rail').children).map((el) =>
      el.getAttribute('data-testid'),
    );
    expect(slots).toEqual([
      'action-rail-avatar',
      'action-rail-like',
      'action-rail-comment',
      'action-rail-follow',
    ]);
  });

  it('关注槽位是空的占位:关注按钮与关注接口是 Task 7 的活,这里不提前接', () => {
    renderRail();

    expect(screen.getByTestId('action-rail-follow')).toBeEmptyDOMElement();
  });

  it('计数是浮在视频上的文字:主文字色 + 与遮罩同源的文字阴影,不赌画面恰好够暗', () => {
    renderRail();

    const count = screen.getByText('3');
    expect(count).toHaveStyle({ color: '#f5f5f7' });
    expect(count.style.textShadow).not.toBe('');
  });

  it('换一条视频(props 变了)时重置本地状态,不把上一条的点赞态留在屏幕上', async () => {
    likeVideoMock.mockResolvedValue({ is_like: true, likes_count: 5 });
    const { rerender } = render(
      <TestRouter>
        <ActionRail video={ITEM} />
      </TestRouter>,
    );

    await userEvent.click(likeButton());
    await waitFor(() => expect(likeButton()).toHaveTextContent(/^5$/));

    // FeedList 若没按 id 加 key,同一个位置就会换成下一条视频
    rerender(
      <TestRouter>
        <ActionRail video={{ ...ITEM, id: 43, is_like: true, likes_count: 9 }} />
      </TestRouter>,
    );

    expect(likeButton()).toHaveTextContent(/^9$/);
    expect(likeButton()).toHaveAttribute('aria-pressed', 'true');
  });
});
