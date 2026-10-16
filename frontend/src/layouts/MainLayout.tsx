import { useEffect, useState, type CSSProperties } from 'react';
import { ConfigProvider, theme } from 'antd';
import { NavLink, Outlet } from 'react-router-dom';
import { getMyProfile } from '../api/auth';
import { FULL_VIEWPORT_HEIGHT, SIDEBAR_WIDTH, darkTokens, spacing } from '../theme/tokens';
import type { UserResp } from '../types/auth';

/** 入口高度:与 FeedList 的"点此重试"(44)、ActionRail 的槽位(48)、VideoPanel 的声音开关(44)
 *  同一个理由 —— 文字才 16px,不留够可点面积,手指点不中。 */
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
        // **任何**失败(401 / 10002 / 5xx / 网络 / 超时)都按"没有这个人"处理 —— 与 router.tsx 的
        // RequireLogin **故意不同**,不是疏漏:
        //   · 这里是**外壳**,它拿登录态只做一件事 —— 决定"个人主页"这个入口指向 /users/{id}
        //     还是 /login(游客没有 id 可拼)。猜错的代价是**一次跳转的目标不对**,用户到了登录页
        //     或到了自己的主页,两处都还能走下去;
        //   · RequireLogin 是**闸门**:它猜错了会把一个已登录的人挡在 /following 外面、送去登录页,
        //     那是"进不去",用户除了骂人没有别的路。所以闸门必须区分"没登录"和"探不通",
        //     而侧栏不必 —— 这里多一个可重试的失败态只会让导航整块不见(那比猜错更糟)。
        //   · 界面立刻是游客态(初值就是 null),这里显式写出来是为了说明"吞掉它是决定,不是忘了":
        //     探测失败不该有任何副作用。
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
    fontSize: 16,
    fontWeight: isActive ? 600 : 400,
    textDecoration: 'none',
    // 底色只在"当前页"这里 inline 给;非当前页留给样式表的 hover(feed.css 的 .feed-nav-link:hover)。
    // 为什么非要让出来:inline 样式优先于样式表,写死成 'transparent' 的话那条 hover 规则永远不会生效。
    // 悬停只动底色(文字色被上面的 inline 钉着,改不了,也不必改:整行提亮一档就是导航最标准的 hover 反馈)。
    background: isActive ? token.colorBgElevated : undefined,
    // 当前页的标记:品牌色竖条 + 亮一档的面 + 加粗。
    // 为什么不是"只换个背景色":#16161c → #1c1c24 只差一档,单靠它太弱,颜色之外还必须有形状。
    // 为什么用 box-shadow 的 inset 而不是 border-left:后者会占宽度、把文字推走,四个入口的字就不对齐了。
    boxShadow: isActive ? `inset 3px 0 0 0 ${token.colorPrimary}` : undefined,
  });

  return (
    // 沉浸底取自 token,不写字面量:内容区若只有半屏内容(空态 / 错误态),
    // 露出来的必须是这个近黑底 —— 来源和别处一样,只有 darkTokens 一处。
    // className=feed-surface 是"这一面"的挂点:焦点环 / 悬停 / 滚动条那几条只能写在样式表里的规则
    // (伪类、伪元素、媒体查询 inline 表达不了)都挂在它下面,见 feed.css。
    <div className="feed-surface" style={{ ...rootStyle, background: token.colorBgLayout }}>
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
            className="feed-nav-link"
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

// 外壳:内容区自己撑满一屏,左侧导航**浮在它上面**(spec §7 / §12 的"满屏出血")。
// 高度来自 theme/tokens.ts 的 FULL_VIEWPORT_HEIGHT —— 与 FeedList 的根、VideoPanel 的面板
// 同一个定义,三处必须相等(理由写在那个常量上)。
//
// 导航的响应式收起:Task 11 **明确决定不做**(不是漏掉的)。375px 视口下它确实占掉 200px(53%),
// 但:① 它自己每一项都是 44px 高、整宽可点,四个入口在该宽度下都点得到,没有"够不着"的东西;
// ② 任何真正的收起方案(收成底部栏 / 图标条 / 汉堡抽屉)都会**作废两条已被复审批准的几何**:
//    视频满屏出血 + 导航浮在其上、以及 VideoPanel 左下角文字那 216px 的左避让 —— 后者假设的正是
//    "左边有一条 200px 的导航",收起之后那 216px 就变成一块空白(窄屏上更明显);
// ③ 本环境没有浏览器,收起的像素结果无法验证,猜错就是把布局改坏。
// 所以它留给真机走查后的后续任务,处理方式(在 ≤767px 收成底部栏 + VideoPanel 左避让随之降到
// spacing.md)与代价都写在 task-11 报告的走查清单里 —— 那是一个需要同时改三个文件的几何决定,
// 不是收尾能顺手带过的。
const rootStyle: CSSProperties = {
  position: 'relative',
  ...FULL_VIEWPORT_HEIGHT,
  width: '100%',
  overflow: 'hidden',
};

// 侧栏是**浮层**(fixed),不占内容区的宽度 —— 视频要铺满整个可视区,导航盖在上面。
// 代价是左下角那几行字会被压住,所以 VideoPanel 的左内边距按 SIDEBAR_WIDTH 避让
// (与它右侧避让操作栏的做法对称);两处宽度共用同一个常量,不能各写一份。
const sidebarStyle: CSSProperties = {
  position: 'fixed',
  top: 0,
  bottom: 0,
  left: 0,
  zIndex: 10,
  width: SIDEBAR_WIDTH,
  display: 'flex',
  flexDirection: 'column',
  // 入口之间只留 4px:每一项自己已经有 44px 高,再多就是散开的按钮墙
  gap: spacing.xxs,
  padding: `${spacing.md}px ${spacing.sm}px`,
};

// 内容区:整宽(满屏出血的视频就落在这里)。
const contentStyle: CSSProperties = {
  height: '100%',
  // 不写 minWidth:0 的话,子项的宽度下限是内容宽度 ——
  // 里面的东西一旦比可用宽度宽,就会撑出横向滚动条。
  minWidth: 0,
};
