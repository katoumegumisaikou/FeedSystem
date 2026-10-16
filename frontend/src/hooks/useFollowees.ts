import { useCallback, useEffect, useSyncExternalStore } from 'react';
import { listFollowees } from '../api/follow';

// 「我关注了谁」的缓存。
//
// 为什么是模块级的一份、而不是每个组件自己 useState:
// 后端**没有**"我是否关注 TA"的读接口,只能拿 GET /users/me/followees 那份全量列表判断。
// 首页一屏一个视频、每条视频一个关注按钮 —— 若各拉一次,同屏就是十几份整份列表的请求,
// 而且同一个作者会被重复拉。所以:全量只拉一次,之后所有按钮读同一份 Set。
//
// 为什么不用 React Context:这份状态不属于任何一棵子树 —— 它就是"当前登录用户"的一个
// 只读视图(和 axios 实例一个性质),而挂 Provider 的位置本该在 Task 8/9 的容器里,
// 本任务不动那些组合点。模块级缓存让任何位置的 FollowButton 直接能用,不必先被包一层。
//
// ⚠️ 这份缓存**与身份无关,而且拉过之后就不再拉**(loaded 是黏的)。所以**身份一变就必须作废**:
//    游客那次静默 401 也会把 loaded 置真 → 不作废的话,用户登录进来后缓存仍是空的且永不重取,
//    每个作者都会永远显示"关注";换账号登录则会沿用上一个人的列表。
//    生产调用点:登录 / 注册成功(见 pages/Login、pages/Register)。**登出接上之后也必须调**
//    —— 目前 api/auth.ts 的 logout() 还没有调用点。
interface FolloweesState {
  ids: Set<number>;
  loading: boolean;
  /** 是否已经尝试拉过(不论成败)。见 fetchFollowees 的注释。 */
  loaded: boolean;
}

const listeners = new Set<() => void>();

let snapshot: FolloweesState = { ids: new Set(), loading: false, loaded: false };

// 在飞的请求。整屏按钮几乎同时挂载,靠它把并发的第一次拉取合流成一条请求。
let inflight: Promise<void> | null = null;

function emit(next: FolloweesState): void {
  snapshot = next;
  listeners.forEach((listener) => listener());
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

// useSyncExternalStore 要求"没变化时返回同一个引用",否则每次渲染都会被判成有新快照。
// 这里的 snapshot 只在 emit 里整体替换,天然满足。
function getSnapshot(): FolloweesState {
  return snapshot;
}

function fetchFollowees(force = false): Promise<void> {
  // 已经有请求在飞:复用它 —— 同屏十几个按钮同时挂载也只会有一条请求
  if (inflight) return inflight;
  // 已经拉过(或已尝试过)就不动:这就是"每个 rail 各拉一次"被挡掉的地方
  if (snapshot.loaded && !force) return Promise.resolve();

  emit({ ...snapshot, loading: true });
  inflight = listFollowees()
    .then((resp) => {
      emit({ ids: new Set(resp.followee_ids), loading: false, loaded: true });
    })
    .catch(() => {
      // 失败:保持原来那份 ids,只把 loading 落下来。错误提示由 api/axios.ts 的拦截器统一弹,
      // 这里再弹一次就是两条 toast。
      //
      // 失败也记 loaded —— 否则每挂上新一屏都会重试一次,首屏十几个 rail 就是一串请求
      // 打同一个接口。重试入口是 refresh()。
      emit({ ...snapshot, loading: false, loaded: true });
    })
    .finally(() => {
      inflight = null;
    });

  return inflight;
}

// 关注 / 取关成功后由 FollowButton 调用:只动这一个 id,不重新拉全量。
// 只为改一个数字就把整份列表(关注上限封顶的那份)重传一遍,是把一次点击放大成一次全表传输;
// 而接口已经回了 following 这个真值,本地照着改就够了。
export function setFollowee(id: number, following: boolean): void {
  const ids = new Set(snapshot.ids);
  if (following) ids.add(id);
  else ids.delete(id);
  // 继续维持"已经拉过"这个事实:改完再拉一次全量就白改了,而且刚关注的人还要等一轮网络
  emit({ ...snapshot, ids, loaded: true });
}

// 让缓存作废:下一次有 FollowButton 挂载时会重新拉一份。
//
// 生产调用点是**身份变化**的地方 —— 登录 / 注册成功(见 pages/Login、pages/Register);
// 登出接上之后也要调。理由见文件头那段 ⚠️。
export function invalidateFollowees(): void {
  inflight = null;
  emit({ ids: new Set(), loading: false, loaded: false });
}

// 仅测试用:语义与 invalidateFollowees 相同,留一个自解释的名字给测试。
// 这份缓存活在模块作用域里,单个测试文件里跨用例不会自动清掉 —— 不还原就会出现
// "后一个用例因为已经拉过而根本不发请求、却拿到前一个用例的 ids"这种假绿灯。
// 生产代码里没有任何调用点。
export function resetFolloweesCacheForTest(): void {
  invalidateFollowees();
}

// 当前"我关注了谁"。
//
// ids 用 Set 而不是数组:每条 rail 都要问一次"这个作者在不在里面",数组是 O(n) 扫,
// 一屏十几条就是十几趟;Set 是 O(1),判断已关注就一行 ids.has(author_id)。
export function useFollowees(): { ids: Set<number>; loading: boolean; refresh: () => void } {
  const state = useSyncExternalStore(subscribe, getSnapshot);

  useEffect(() => {
    // 挂载时问一句"要不要拉"。真正决定发不发请求的是模块级的 inflight / loaded,
    // 所以同屏 N 个按钮总共只有一条请求 —— 这就是"拉一次、大家共用"的落点。
    void fetchFollowees();
  }, []);

  const refresh = useCallback(() => {
    // 手动重拉一次(force):拉失败之后按钮上看不出任何异常,重试入口只有调用方手上这个。
    void fetchFollowees(true);
  }, []);

  return { ids: state.ids, loading: state.loading, refresh };
}
