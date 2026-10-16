import { useEffect, useState, type ReactNode } from 'react';
import { Navigate, Route, Routes, useLocation } from 'react-router-dom';
import { getMyProfile } from './api/auth';
import MainLayout from './layouts/MainLayout';
import FollowingPage from './pages/Following';
import ForgotPasswordPage from './pages/ForgotPassword';
import HomePage from './pages/Home';
import HotPage from './pages/Hot';
import LoginPage from './pages/Login';
import RegisterPage from './pages/Register';
import UserProfilePage from './pages/UserProfile';

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

// 登录态探测的三种情形。用三态而不是两个布尔:"还没有结论"必须是一个**独立的**状态 ——
// 否则"还没探完"和"确实没登录"分不开,会在探测期间先把不该看的页面放出去一帧。
type ProbeState = 'probing' | 'in' | 'out';

/** 需登录的路由守卫:登录态以一次静默探测为准;未登录送回登录页,并把原址带上。 */
function RequireLogin({ children }: { children: ReactNode }) {
  const location = useLocation();
  const [probe, setProbe] = useState<ProbeState>('probing');

  useEffect(() => {
    // silent: 是必须的 —— 访客打开 /following 是最常走的那条路径,401 不该弹一句"未登录"
    // (与 MainLayout 的探测同一口径)。
    //
    // 为什么这里自己探一次,而不是读外壳那一份:外壳没有把"我是谁"暴露出来(它的状态只服务于导航),
    // 而放行与否必须用**新鲜的**事实 —— 登录成功后 cookie 才落地,缓存下来的"未登录"
    // 会让用户登录完又被弹回登录页。代价是访问 /following 会多一次静默探测(游客那一次是 401),
    // 记在案:真要省掉它,得让外壳把登录态通过 Outlet context 传下来,那是外壳的改动,不在本任务里。
    let cancelled = false;

    getMyProfile({ silent: true })
      .then(() => {
        if (!cancelled) setProbe('in');
      })
      .catch(() => {
        // 401 / 网络失败都按"没有登录"处理:这是个守卫,拿不准的时候只能更保守。
        if (!cancelled) setProbe('out');
      });

    return () => {
      cancelled = true;
    };
  }, []);

  // 探测没有结论:什么都不渲染。既不能放行(可能是游客),也不能先闪一下登录页
  // (已登录的人会白看一眼登录表单,再被打回原页)。
  if (probe === 'probing') return null;

  if (probe === 'out') {
    // replace:守卫的跳转不该在历史里留一格 —— 否则按返回又回到那个进不去的页面,来回弹。
    // 原址放在 router state 里而不是查询参数:地址栏不被一个"跳转用"的参数污染(理由详见 pages/Login)。
    return <Navigate to="/login" replace state={{ from: location.pathname + location.search }} />;
  }

  return <>{children}</>;
}
