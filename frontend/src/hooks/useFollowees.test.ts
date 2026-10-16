import { act, render, screen, waitFor } from '@testing-library/react';
import { createElement } from 'react';
import { listFollowees } from '../api/follow';
import { resetFolloweesCacheForTest, setFollowee, useFollowees } from './useFollowees';

// 这一层要证明的是「整屏只拉一次全量、之后靠本地缓存维护」这条策略,而不是"hook 调了接口":
//   ① followee_ids 收进 Set,判断已关注只看这个 Set;
//   ② 同屏 N 个挂载点(每条视频一个关注按钮)总共只发一次请求,后挂上的不再打接口;
//   ③ loading 如实反映在飞的请求;
//   ④ 失败不炸、也不会变成"每挂一个新 rail 就重试一次"的请求放大器;
//   ⑤ 关注 / 取关成功后本地改缓存,所有挂载点立刻跟着变 —— 不重新拉全量。
//
// api 一律 mock:HTTP 的路径 / 方法由 api/follow.test.ts 负责,这里只关心拿到响应之后的状态。
vi.mock('../api/follow', () => ({
  listFollowees: vi.fn(),
  followUser: vi.fn(),
  unfollowUser: vi.fn(),
}));

const listFolloweesMock = vi.mocked(listFollowees);

beforeEach(() => {
  listFolloweesMock.mockReset();
  // 这份缓存活在模块作用域里(见 useFollowees.ts),跨用例不会自动清掉。
  // 不还原的话,后一个用例会因为"已经拉过了"而根本不发请求,却拿到前一个用例的 ids —— 假绿灯。
  resetFolloweesCacheForTest();
});

// 探针:把 hook 的返回值画进 DOM,断言的是"界面上看到的",不是内部变量。
// 用 createElement 而不是 JSX,是为了让这个文件保持 .ts(与 brief 给的文件名一致)。
function Probe({ id }: { id: number }) {
  const { ids, loading, refresh } = useFollowees();
  return createElement(
    'span',
    { 'data-testid': `probe-${id}` },
    ids.has(id) ? '已关注' : '未关注',
    loading ? '/加载中' : '/就绪',
    createElement('button', { key: 'refresh', onClick: refresh }, `刷新${id}`),
  );
}

function renderProbes(...ids: number[]) {
  return render(
    createElement('div', null, ...ids.map((id) => createElement(Probe, { key: id, id }))),
  );
}

describe('useFollowees 的全量缓存', () => {
  it('把 followee_ids 收进 Set:目标在里面 → 已关注,不在 → 未关注', async () => {
    listFolloweesMock.mockResolvedValue({ followee_ids: [7], limit: 5000 });

    renderProbes(7, 9);

    // 列表还没回来时不许凭猜测说"已关注" —— 按钮要禁用,靠 loading 表达
    expect(screen.getByTestId('probe-7')).toHaveTextContent('未关注/加载中');

    await waitFor(() => expect(screen.getByTestId('probe-7')).toHaveTextContent('已关注/就绪'));
    expect(screen.getByTestId('probe-9')).toHaveTextContent('未关注/就绪');
  });

  it('多个挂载点共用一次请求:每个 rail 各拉一次会把接口打穿', async () => {
    listFolloweesMock.mockResolvedValue({ followee_ids: [7], limit: 5000 });

    const view = renderProbes(7);
    await waitFor(() => expect(screen.getByTestId('probe-7')).toHaveTextContent('已关注/就绪'));
    expect(listFolloweesMock).toHaveBeenCalledTimes(1);

    // 继续挂上第二个、第三个关注按钮(滚动到下一屏就会发生)
    listFolloweesMock.mockClear();
    view.rerender(
      createElement(
        'div',
        null,
        ...[7, 9, 11].map((id) => createElement(Probe, { key: id, id })),
      ),
    );

    await waitFor(() => expect(screen.getByTestId('probe-11')).toHaveTextContent('未关注/就绪'));
    // 已经拉过一次,后来者复用缓存,一次请求都不该再发
    expect(listFolloweesMock).not.toHaveBeenCalled();
  });

  it('loading 如实反映在飞的请求', async () => {
    let settle: (resp: { followee_ids: number[]; limit: number }) => void = () => {};
    listFolloweesMock.mockReturnValue(
      new Promise((resolve) => {
        settle = resolve;
      }),
    );

    renderProbes(7);
    expect(screen.getByTestId('probe-7')).toHaveTextContent('未关注/加载中');

    await act(async () => {
      settle({ followee_ids: [7], limit: 5000 });
    });

    expect(screen.getByTestId('probe-7')).toHaveTextContent('已关注/就绪');
  });

  it('拉失败:保持未关注、loading 落回,之后 refresh 能重试成功', async () => {
    listFolloweesMock.mockRejectedValueOnce(new Error('boom'));
    listFolloweesMock.mockResolvedValueOnce({ followee_ids: [7], limit: 5000 });

    renderProbes(7);

    await waitFor(() => expect(screen.getByTestId('probe-7')).toHaveTextContent('未关注/就绪'));
    expect(listFolloweesMock).toHaveBeenCalledTimes(1);

    // 错误提示由 api/axios.ts 的拦截器统一弹,这里只负责"把重试入口留给 refresh"
    await act(async () => {
      screen.getByRole('button', { name: '刷新7' }).click();
    });

    await waitFor(() => expect(screen.getByTestId('probe-7')).toHaveTextContent('已关注/就绪'));
    expect(listFolloweesMock).toHaveBeenCalledTimes(2);
  });

  it('失败也算"拉过了":不会每挂上新一屏就重试一次', async () => {
    listFolloweesMock.mockRejectedValue(new Error('boom'));

    renderProbes(7);
    await waitFor(() => expect(screen.getByTestId('probe-7')).toHaveTextContent('未关注/就绪'));
    expect(listFolloweesMock).toHaveBeenCalledTimes(1);

    // 首屏十几屏、每屏一个关注按钮:失败后若每次挂载都重试,就是一串请求打同一个接口
    renderProbes(9);

    await waitFor(() => expect(screen.getByTestId('probe-9')).toHaveTextContent('未关注/就绪'));
    expect(listFolloweesMock).toHaveBeenCalledTimes(1);
  });

  it('setFollowee 本地改缓存:所有挂载点立刻跟着变,且不重拉全量', async () => {
    listFolloweesMock.mockResolvedValue({ followee_ids: [], limit: 5000 });

    renderProbes(7, 9);
    await waitFor(() => expect(screen.getByTestId('probe-7')).toHaveTextContent('未关注/就绪'));

    act(() => setFollowee(7, true));
    expect(screen.getByTestId('probe-7')).toHaveTextContent('已关注/就绪');
    // 只动了被点的那一个 id,别的按钮不受影响
    expect(screen.getByTestId('probe-9')).toHaveTextContent('未关注/就绪');

    act(() => setFollowee(7, false));
    expect(screen.getByTestId('probe-7')).toHaveTextContent('未关注/就绪');

    expect(listFolloweesMock).toHaveBeenCalledTimes(1);
  });
});
