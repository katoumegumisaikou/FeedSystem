import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { followUser, listFollowees, unfollowUser } from '../../api/follow';
import { resetFolloweesCacheForTest } from '../../hooks/useFollowees';
import FollowButton from './FollowButton';

// 这个文件要证明的是**行为**,不是"props 传进去了":
//   ① 已关注 / 未关注由「全量关注列表」判断(后端没有"我是否关注 TA"的读接口);
//   ② 点下去打的是与当前状态匹配的那个接口(POST 关注 / DELETE 取关);
//   ③ 界面跟着**响应里的 following** 走,不是自己取反(关注有上限,服务端可能拒绝);
//   ④ 关注成功后只改本地那份缓存,不为了刷新状态再拉一次全量。
//
// api 一律 mock:这里不测 HTTP,路径 / 方法由 api/follow.test.ts 负责。
vi.mock('../../api/follow', () => ({
  listFollowees: vi.fn(),
  followUser: vi.fn(),
  unfollowUser: vi.fn(),
}));

const listFolloweesMock = vi.mocked(listFollowees);
const followUserMock = vi.mocked(followUser);
const unfollowUserMock = vi.mocked(unfollowUser);

// 全量列表的缓存活在模块作用域里,跨用例不会自动清掉 —— 不还原就会出现
// "这个用例没发请求、却拿到上个用例的 ids"的假绿灯。
beforeEach(() => {
  listFolloweesMock.mockReset();
  followUserMock.mockReset();
  unfollowUserMock.mockReset();
  resetFolloweesCacheForTest();
  // 默认:我谁都没关注
  listFolloweesMock.mockResolvedValue({ followee_ids: [], limit: 5000 });
});

const button = () => screen.getByTestId('follow-button');

// 按钮在"全量列表还没回来"时是禁用的(见 FollowButton.tsx)。要点击就得先等它可用。
async function renderLoaded(authorId: number) {
  render(<FollowButton authorId={authorId} />);
  await waitFor(() => expect(button()).toBeEnabled());
}

describe('FollowButton 的已关注态来自全量关注列表', () => {
  it('目标在 followee_ids 里 → 显示"已关注"', async () => {
    listFolloweesMock.mockResolvedValue({ followee_ids: [77], limit: 5000 });

    render(<FollowButton authorId={77} />);

    await waitFor(() => expect(button()).toHaveTextContent('已关注'));
  });

  it('目标不在 followee_ids 里 → 显示"关注"', async () => {
    listFolloweesMock.mockResolvedValue({ followee_ids: [77], limit: 5000 });

    render(<FollowButton authorId={78} />);

    await waitFor(() => expect(button()).toBeEnabled());
    expect(button()).toHaveTextContent('关注');
    expect(button()).not.toHaveTextContent('已关注');
  });

  it('同屏两个按钮共用一次全量请求(每个 rail 各拉一次会把接口打穿)', async () => {
    listFolloweesMock.mockResolvedValue({ followee_ids: [77], limit: 5000 });

    render(
      <>
        <FollowButton authorId={77} />
        <FollowButton authorId={78} />
      </>,
    );

    await waitFor(() => expect(screen.getAllByTestId('follow-button')[1]).toBeEnabled());

    const [followed, notFollowed] = screen.getAllByTestId('follow-button');
    expect(followed).toHaveTextContent('已关注');
    expect(notFollowed).toHaveTextContent('关注');
    expect(listFolloweesMock).toHaveBeenCalledTimes(1);
  });
});

describe('FollowButton 的接口接线', () => {
  it('未关注时点击 → 关注接口,界面跟着响应变"已关注"', async () => {
    followUserMock.mockResolvedValue({ following: true, follower_count: 1 });

    await renderLoaded(77);

    await userEvent.click(button());

    expect(followUserMock).toHaveBeenCalledWith(77);
    expect(unfollowUserMock).not.toHaveBeenCalled();

    await waitFor(() => expect(button()).toHaveTextContent('已关注'));
  });

  it('已关注时点击 → 取关接口,界面跟着响应变回"关注"', async () => {
    listFolloweesMock.mockResolvedValue({ followee_ids: [77], limit: 5000 });
    unfollowUserMock.mockResolvedValue({ following: false, follower_count: 0 });

    await renderLoaded(77);
    await waitFor(() => expect(button()).toHaveTextContent('已关注'));

    await userEvent.click(button());

    expect(unfollowUserMock).toHaveBeenCalledWith(77);
    expect(followUserMock).not.toHaveBeenCalled();

    await waitFor(() => expect(button()).toHaveTextContent('关注'));
  });

  it('状态以响应里的 following 为准:服务端回 false(比如到了关注上限)就不许显示"已关注"', async () => {
    followUserMock.mockResolvedValue({ following: false, follower_count: 0 });

    await renderLoaded(77);

    await userEvent.click(button());
    // 等这次请求落地
    await act(async () => {});

    expect(followUserMock).toHaveBeenCalledWith(77);
    expect(button()).toHaveTextContent('关注');
    expect(button()).not.toHaveTextContent('已关注');
  });

  it('关注成功后只改本地缓存:不为了刷新状态再拉一次全量', async () => {
    followUserMock.mockResolvedValue({ following: true, follower_count: 1 });

    await renderLoaded(77);
    expect(listFolloweesMock).toHaveBeenCalledTimes(1);

    await userEvent.click(button());
    await waitFor(() => expect(button()).toHaveTextContent('已关注'));

    expect(listFolloweesMock).toHaveBeenCalledTimes(1);
  });

  it('请求还在飞的时候再点不会重复发请求(方向相反的两次响应会互相覆盖)', async () => {
    let settle: (resp: { following: boolean; follower_count: number }) => void = () => {};
    followUserMock.mockReturnValue(
      new Promise((resolve) => {
        settle = resolve;
      }),
    );

    await renderLoaded(77);

    await userEvent.click(button());
    await userEvent.click(button());

    expect(followUserMock).toHaveBeenCalledTimes(1);

    await act(async () => {
      settle({ following: true, follower_count: 1 });
    });

    expect(button()).toHaveTextContent('已关注');
  });

  it('关注失败保持原样,不弹面板 —— 错误提示归 axios 拦截器,组件不重复弹', async () => {
    followUserMock.mockRejectedValue(new Error('boom'));

    await renderLoaded(77);

    await userEvent.click(button());
    await act(async () => {});

    expect(button()).toHaveTextContent('关注');
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('全量列表拉失败时按钮仍可用:按当前(空)缓存显示"关注",关注成功后本地纠正', async () => {
    listFolloweesMock.mockRejectedValue(new Error('boom'));
    followUserMock.mockResolvedValue({ following: true, follower_count: 1 });

    await renderLoaded(77);
    expect(button()).toHaveTextContent('关注');

    await userEvent.click(button());

    await waitFor(() => expect(button()).toHaveTextContent('已关注'));
  });
});

describe('FollowButton 的可达性', () => {
  it('是一个真按钮:键盘 / 读屏可达,状态靠 aria-pressed 表达', async () => {
    listFolloweesMock.mockResolvedValue({ followee_ids: [77], limit: 5000 });

    render(<FollowButton authorId={77} />);
    await waitFor(() => expect(button()).toHaveAttribute('aria-pressed', 'true'));

    expect(button()).toHaveRole('button');
    // 可见文字就是可访问名(标签与名字一致,读屏念的和眼睛看到的不会打架)
    expect(screen.getByRole('button', { name: '已关注' })).toBe(button());
  });
});
