import { useEffect, useState, type CSSProperties } from 'react';
import { ConfigProvider, theme } from 'antd';
import { NavLink, Outlet } from 'react-router-dom';
import { getMyProfile } from '../api/auth';
import { darkTokens, spacing } from '../theme/tokens';
import type { UserResp } from '../types/auth';

/** 左侧栏宽度:四个两字入口 + 12px 内边距放得下,再窄就有换行风险。 */
const SIDEBAR_WIDTH = 200;

/** 入口高度:与 FeedList 的"点此重试"(44)、ActionRail 的槽位(48)同一个理由 ——
 *  文字才 15px,不留够可点面积,手指点不中。 */
const ENTRY_MIN_HEIGHT = 44;

// feed 页面的外壳:左侧栏 + 内容区。
//
// 深色只落在**这个子树**里 —— 由这里的嵌套 <ConfigProvider theme={darkTokens}> 落地(R1)。
// 为什么不在 main.tsx 的根 ConfigProvider 上挂:那会把登录 / 注册 / 个人主页一起刷黑,
// 而 spec §7 要的是"两套页面同源(同一份品牌色与刻度),但深浅各自成立"。
// 嵌套只覆盖 darkTokens 里显式写出的那些 token,父层(浅色)拿不到任何一项 ——
// theme/tokens.test.tsx 已经钉住了这个行为,这里只是第一次真用上它。
export default function MainLayout() {
  return (
    <ConfigProvider theme={darkTokens}>
      <Shell />
    </ConfigProvider>
  );
}

// 为什么拆出这一层,而不是把 ConfigProvider 和侧栏写在同一个函数的返回里:
// theme.useToken() 读的是**最近的** ConfigProvider —— 只有在它的子树里调用,
// 才拿得到 darkTokens 那一套;写成同一个函数就得把颜色硬编码,接线也就白接了。
function Shell() {
  const { token } = theme.useToken();

  // 当前登录用户;null = 游客(也可能是探测还没回来)。
  const [me, setMe] = useState<UserResp | null>(null);

  useEffect(() => {
    // 卸载后不再落状态。跳到一个不进外壳的页面(比如 /login,Task 10 的路由)
    // 会把整棵外壳卸掉,而这次探测可能还在飞 —— 一次迟到的响应没有地方可写。
    let cancelled = false;

    // silent: true 是必须的 —— 这是"后台探测登录态",访客可能只是打开首页看视频,
    // 既没点关注也没进个人主页,凭什么被弹一句「未登录」。silent 只挡了 axios 拦截器的
    // 提示;下面这条 catch 也不能再弹任何东西,游客态不是错误。
    getMyProfile({ silent: true })
      .then((user) => {
        if (!cancelled) setMe(user);
      })
      .catch(() => {
        // 401 / 网络失败都按"没有这个人"处理:界面立刻就是游客态(初值就是 null),
        // 这里显式写出来是为了说明"吞掉它是决定,不是忘了" —— 探测失败不该有任何副作用。
        if (!cancelled) setMe(null);
      });

    return () => {
      cancelled = true;
    };
  }, []);

  // 四个入口(spec §6:主页 / 热门 / 关注 / 个人主页)。
  // "个人主页"是唯一随登录态变化的一项:游客没有 id 可拼,只能去登录页(spec §8);
  // 而个人主页本身**不进外壳**(spec §6,它是现有的浅色独立页),所以点它会跳出去。
  const entries: { label: string; to: string; end?: boolean }[] = [
    // end:只有它必须精确匹配 —— 不写的话 "/" 会匹配上任意路径,
    // 于是"主页"在任何页面都算当前页,高亮就失去了意义。
    { label: '主页', to: '/', end: true },
    { label: '热门', to: '/hot' },
    { label: '关注', to: '/following' },
    { label: '个人主页', to: me ? `/users/${me.id}` : '/login' },
  ];

  // 入口样式随"是不是当前页"变。颜色全部取自 token(不写死字面量):
  // 这样"深色只作用于此"这件事只有 token 一个来源,将来调色也只改一处。
  const entryStyle = (isActive: boolean): CSSProperties => ({
    display: 'flex',
    alignItems: 'center',
    minHeight: ENTRY_MIN_HEIGHT,
    padding: `0 ${spacing.sm}px`,
    borderRadius: token.borderRadius,
    color: isActive ? token.colorText : token.colorTextSecondary,
    fontSize: 15,
    fontWeight: isActive ? 600 : 400,
    textDecoration: 'none',
    background: isActive ? token.colorBgElevated : 'transparent',
    // 当前页的标记:品牌色竖条 + 亮一档的面 + 加粗。
    // 为什么不是"只换个背景色":#16161c → #1c1c24 只差一档,单靠它太弱,颜色之外还必须有形状。
    // 为什么用 box-shadow 的 inset 而不是 border-left:后者会占宽度、把文字推走,四个入口的字就不对齐了。
    boxShadow: isActive ? `inset 3px 0 0 0 ${token.colorPrimary}` : undefined,
  });

  return (
    // 沉浸底取自 token,不写字面量:内容区若只有半屏内容(空态 / 错误态),
    // 露出来的必须是这个近黑底 —— 来源和别处一样,只有 darkTokens 一处。
    <div style={{ ...rootStyle, background: token.colorBgLayout }}>
      <nav
        aria-label="主导航"
        style={{
          ...sidebarStyle,
          background: token.colorBgContainer,
          borderRight: `1px solid ${token.colorBorder}`,
        }}
      >
        {entries.map((entry) => (
          <NavLink
            key={entry.label}
            to={entry.to}
            // 只有主页给 true。不传时 NavLink 走的是模糊匹配 —— 对 "/" 来说那等于
            // "我在任何路径下都算当前页",高亮就废了(见上面 entries 里的注释)。
            end={entry.end}
            style={({ isActive }) => entryStyle(isActive)}
          >
            {entry.label}
          </NavLink>
        ))}
      </nav>

      <main style={contentStyle}>
        <Outlet />
      </main>
    </div>
  );
}

// 外壳:横向两栏(侧栏 + 内容区),自己撑满一屏。
const rootStyle: CSSProperties = {
  display: 'flex',
  minHeight: '100vh',
  width: '100%',
};

// 侧栏是**在流里的**一列,不是浮在视频上的浮层(spec §7 说的"浮"落在右边那条操作栏上):
// VideoPanel 的作者名 / 标题是左下角贴着左边 16px 的,一条 200px 宽、满高的浮层盖上去,
// 正好把那几行字压住 —— 那是"外壳把内容挡了",不是设计。所以侧栏占布局空间,
// 内容区让出这一列,视频仍铺满剩下的整个可视区。
const sidebarStyle: CSSProperties = {
  // 内容区变窄时不许挤走侧栏(宽度是设计值,不是可伸缩的)
  flex: '0 0 auto',
  width: SIDEBAR_WIDTH,
  display: 'flex',
  flexDirection: 'column',
  // 入口之间只留 4px:每一项自己已经有 44px 高,再多就是散开的按钮墙
  gap: spacing.xxs,
  padding: `${spacing.md}px ${spacing.sm}px`,
};

// 内容区:吃掉剩下的宽度(满屏出血的视频就落在这里)。
const contentStyle: CSSProperties = {
  flex: 1,
  // 不写 minWidth:0 的话,flex 子项的宽度下限是内容宽度 ——
  // 内容区里的东西一旦比可用宽度宽,就会把侧栏顶出视口。
  minWidth: 0,
};
