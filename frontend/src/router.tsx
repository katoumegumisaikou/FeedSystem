import { useEffect, useState, type CSSProperties, type ReactNode } from 'react';
import { Navigate, Route, Routes, useLocation } from 'react-router-dom';
import { getMyProfile } from './api/auth';
import { isUnauthorized } from './api/axios';
import MainLayout from './layouts/MainLayout';
import FollowingPage from './pages/Following';
import ForgotPasswordPage from './pages/ForgotPassword';
import HomePage from './pages/Home';
import HotPage from './pages/Hot';
import LoginPage from './pages/Login';
import RegisterPage from './pages/Register';
import UserProfilePage from './pages/UserProfile';
import { SIDEBAR_WIDTH, palette, spacing } from './theme/tokens';

export function AppRouter() {
  return (
    <Routes>
      {/* 三个流页面挂在**同一个外壳**下(嵌套路由:MainLayout 的 <Outlet/> 就是它们的位置)。
          外壳是深色的、导航浮在内容之上 —— 三个页面共享同一套几何,所以它们的差别只有
          "拉哪一条流、空的时候说什么"。 */}
      <Route element={<MainLayout />}>
        <Route path="/" element={<HomePage />} />
        <Route path="/hot" element={<HotPage />} />
        {/* 关注流要登录(后端 GET /videos/following 挂了 SetSensitive,无 token 直接 401):
            游客先送到登录页,并把原址带上,登录成功后回到这里(spec §11)。 */}
        <Route
          path="/following"
          element={
            <RequireLogin>
              <FollowingPage />
            </RequireLogin>
          }
        />
      </Route>

      {/* 账号页与个人主页**不进外壳**(spec §6):它们是现有的浅色独立页,
          套进深色的浮层导航里就串味了(深色是由外壳的嵌套 ConfigProvider 落地的)。 */}
      <Route path="/login" element={<LoginPage />} />
      <Route path="/register" element={<RegisterPage />} />
      <Route path="/forgot-password" element={<ForgotPasswordPage />} />
      {/* :id 是动态段,匹配 /users/1、/users/42……页面里用 useParams 取出来 */}
      <Route path="/users/:id" element={<UserProfilePage />} />
      {/* 认不出来的路径回主页,不再回登录页:登录页已经不是这个应用的门面 ——
          打开 / 就能看视频,把打错的地址送进登录表单只会让人以为走错了地方。 */}
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  );
}

// 登录态探测的四种情形。用四态而不是"两个布尔 + 一个 catch 兜底":
//   · "还没有结论"必须是一个**独立的**状态 —— 否则"还没探完"和"确实没登录"分不开,
//     会在探测期间先把不该看的页面放出去一帧;
//   · "探测**失败**"(5xx / 网络 / 超时)也必须是独立的一态 —— 它不是"没登录",
//     见 RequireLogin 里的分类。
type ProbeState = 'probing' | 'in' | 'out' | 'error';

// 四态文案取自 spec §13 那张表(与 FeedList 逐字一致),不新造句子:
// 探测是"加载",探不通是"加载失败,点此重试"——同一件事在两个地方就该是同一句话。
const LOADING_TEXT = '加载中…';
const RETRY_TEXT = '加载失败,点此重试';

/** 需登录的路由守卫:登录态以一次静默探测为准。
 *
 *  只有 401 / 业务码 10002(见 api/axios.ts 的 isUnauthorized)才算"确定没登录"⇒ 送回登录页
 *  并把原址带上;其余失败(5xx / 网络故障 / 10s 超时)是"探不通",给一个**可重试的失败态**。
 *  为什么非分不可:把"探不通"当成"没登录",一个已登录的人打开 /following 会被送进登录页,
 *  而屏幕上没有任何一句话解释这件事 —— 那看起来就是应用坏了(而且登录页会因同一个原因失败)。 */
function RequireLogin({ children }: { children: ReactNode }) {
  const location = useLocation();
  const [probe, setProbe] = useState<ProbeState>('probing');
  // 重试:改一次这个计数,下面的探测 effect 就再跑一遍(不为此另造一个状态机)。
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    // 卸载后不再落状态 —— 一次迟到的响应没有地方可写。
    let cancelled = false;
    // 重试进来时先回到"探测中":上一轮那个失败态不该在探测期间继续摆着。
    setProbe('probing');

    // silent: 是必须的 —— 访客打开 /following 是最常走的那条路径,401 不该弹一句"未登录"
    // (与 MainLayout 的探测同一口径)。
    //
    // 为什么这里自己探一次,而不是读外壳那一份:外壳没有把"我是谁"暴露出来(它的状态只服务于导航),
    // 而放行与否必须用**新鲜的**事实 —— 登录成功后 cookie 才落地,缓存下来的"未登录"
    // 会让用户登录完又被弹回登录页。代价是访问 /following 会多一次静默探测(游客那一次是 401),
    // 记在案:真要省掉它,得让外壳把登录态通过 Outlet context 传下来,那是外壳的改动,不在本任务里。
    getMyProfile({ silent: true })
      .then(() => {
        if (!cancelled) setProbe('in');
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setProbe(isUnauthorized(err) ? 'out' : 'error');
      });

    return () => {
      cancelled = true;
    };
  }, [attempt]);

  // 探测没有结论:渲染一句"加载中…",而不是 null。
  // 为什么不能返回 null:这里在深色外壳里(近黑底),什么都不画就是一片黑 —— 最坏能黑到
  // axios 的 10s 超时,已登录的用户看到的是"页面坏了",而不是"在加载"。
  // 为什么不是"先渲染登录页":已登录的人会白看一眼登录表单,再被打回原页;
  // 为什么不是"先放行":可能是游客,关注流那一页会立刻拿一个 401。
  // role=status ⇒ 读屏也会听到(这是"还没到"这个状态唯一能被播报的形式)。
  if (probe === 'probing') {
    return (
      <div data-testid="route-guard-status" role="status" style={guardStatusStyle}>
        {LOADING_TEXT}
      </div>
    );
  }

  // 探不通(不是"没登录"):**不跳转**,给一个能重试的出口。
  // 这里不套 role="status":里面是一个真按钮,由它自己说话(与 FeedList 的失败态同一条理由,
  // 也不该把一个可聚焦控件塞进 live region)。
  if (probe === 'error') {
    return (
      <div data-testid="route-guard-status" style={guardStatusStyle}>
        <button
          type="button"
          data-testid="route-guard-retry"
          onClick={() => setAttempt((n) => n + 1)}
          style={guardRetryStyle}
        >
          {RETRY_TEXT}
        </button>
      </div>
    );
  }

  if (probe === 'out') {
    // replace:守卫的跳转不该在历史里留一格 —— 否则按返回又回到那个进不去的页面,来回弹。
    // 原址放在 router state 里而不是查询参数:地址栏不被一个"跳转用"的参数污染(理由详见 pages/Login)。
    // 这套机制与 FollowButton 的"游客点关注"共用(同一种 state 形状,登录页只认一份)。
    return <Navigate to="/login" replace state={{ from: location.pathname + location.search }} />;
  }

  return <>{children}</>;
}

// 守卫那两帧(探测中 / 探不通)的几何与颜色:与 FeedList 的状态浮层同一套 —— 同样挂在深色
// 外壳里,同样要让开左侧那条 fixed 导航(SIDEBAR_WIDTH),否则窄屏下居中的那行字正好压在导航底下。
// 颜色走 palette、数字走 spacing/SIDEBAR_WIDTH:深色只有 theme/tokens.ts 一个来源。
const guardStatusStyle: CSSProperties = {
  position: 'absolute',
  top: 0,
  right: 0,
  bottom: 0,
  left: SIDEBAR_WIDTH,
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  padding: spacing.lg,
  textAlign: 'center',
  color: palette.textSecondary,
  fontSize: 14,
};

// 重试按钮:与 FeedList 的"点此重试"同一把尺子(44 高、整条可点、下划线是唯一的"能点"提示),
// 因为它是这里唯一的出路。
const guardRetryStyle: CSSProperties = {
  display: 'inline-flex',
  alignItems: 'center',
  minHeight: 44,
  color: palette.textPrimary,
  fontSize: 14,
  fontWeight: 600,
  fontFamily: 'inherit',
  padding: `${spacing.xxs}px 0`,
  border: 'none',
  background: 'none',
  textDecoration: 'underline',
  cursor: 'pointer',
};
