import type { ThemeConfig } from 'antd';

// 深色设计 token —— spec §13「深色 token 起始值」那张表落成的 antd 形态。
//
// 为什么是一份独立的 ThemeConfig、而不是改 main.tsx 里的全局 ConfigProvider:
// 深色只属于新的 feed 页面(登录 / 注册 / 个人主页保持浅色),所以由 Task 9 的 MainLayout
// 在自己的子树里嵌套 <ConfigProvider theme={darkTokens}>。嵌套只覆盖这里显式写出的 token,
// 父层那些浅色页拿不到任何一项,也就不会被刷黑。
//
// 为什么只写 token、不挂 darkAlgorithm:决定性理由是 —— **darkAlgorithm 会把 colorPrimary
// 从 #1677ff 改成 #1668dc**(实测),那等于新造了一个强调色,违反"与浅色页共用同一品牌色"。
// (次要理由:算法会沿 ConfigProvider 树影响所有后代,包括将来任何嵌套子树。)
//
// 代价是没显式写的 token 仍按「浅色算法 + 深色种子」推,而其中几项推出来是坏的(实测):
//   - colorBorder / colorBorderSecondary 推成纯黑 #000000(antd 用 getSolidColor 把底色继续
//     压暗,而 #0b0b0f 已近纯黑)→ 等于没有描边
//   - colorSplit 推成 rgba(0,0,0,0.99) → 分隔线不可见(Divider 用的是 colorSplit,不是
//     colorBorderSecondary;后者管 Card / Table / Tabs 之类的边框)
//   - colorBgLayout 推成 #020204 → 比 spec §13 的沉浸底 #0b0b0f 还黑
//   - colorBgSpotlight 推成 rgba(245,245,247,0.85)(近白),而气泡文字是 colorTextLightSolid
//     (#fff)→ 白字白底,几乎不可读
// 以上几项都已显式钉住(见下,且有断言守着)。其余推导值属 antd 常态(如 colorTextQuaternary
// ≈2.34:1 是"禁用态"的常见强度),可用,但不能拿来承载正文或占位说明。

/** 间距刻度:4 / 8 / 12 / 16 / 24 / 36(spec §13)。antd 没有 spacing token,只能自己给。 */
export const spacing = {
  /** 4 —— 图标与文字之间 */
  xxs: 4,
  /** 8 —— 控件内部间距 */
  xs: 8,
  /** 12 —— 相邻控件之间 */
  sm: 12,
  /** 16 —— 面板 / 卡片内边距 */
  md: 16,
  /** 24 —— 区块之间 */
  lg: 24,
  /** 36 —— 页面级留白 */
  xl: 36,
} as const;

/** 左侧浮层导航的宽度。VideoPanel 用同一个常量给左下角文字留避让 ——
 *  两处必须同源:各写一份的话,作者名要么被压在导航底下,要么空出一大块。 */
export const SIDEBAR_WIDTH = 200;

/** 「一屏」的高度:外壳(MainLayout)、流的根(FeedList)、单个视频面板(VideoPanel)同用一份。
 *
 *  为什么要有这个常量:这三处的高度**必须相等** —— 面板是"一屏一屏"吸附的,外壳与流的根
 *  也各自撑满一屏;只要有一处差一条,视频就会跟外壳错开(底部露出一条底色,吸附也对不齐)。
 *  之前这三个值分别写在三个文件里,改一处忘一处就是这种错;现在定义只有这一份。
 *
 *  为什么是这一对值:100vh 是"大视口高" —— 移动端浏览器带地址栏时它比真正看得见的高度大,
 *  底部那排(作者名 / 操作栏)会被压到地址栏下面;100dvh 是动态视口高,跟着地址栏伸缩。
 *  不支持 dvh 的老浏览器忽略 maxHeight(它只是把高度**夹住**,不是拉伸),自然退回 100vh。 */
export const FULL_VIEWPORT_HEIGHT = {
  height: '100vh',
  maxHeight: '100dvh',
} as const;

export const darkTokens: ThemeConfig = {
  token: {
    // 背景(沉浸底)与表面(侧栏 / 面板)—— spec §13 的两个底色。
    colorBgBase: '#0b0b0f',
    colorBgContainer: '#16161c',
    // 浮层(下拉、弹层、气泡)要比面板再亮一档,否则弹层与面板糊在一起分不出层级。
    colorBgElevated: '#1c1c24',
    // 描边与分隔线:必须显式给 —— 见文件头,推导值是纯黑 #000000 / rgba(0,0,0,0.99),等于没有。
    colorBorder: '#2c2c36',
    colorBorderSecondary: '#2c2c36',
    colorSplit: 'rgba(245,245,247,0.10)',
    // 布局底:推导值 #020204 比 spec §13 的沉浸底还黑,显式对齐。
    colorBgLayout: '#0b0b0f',
    // 气泡/浮窗底(Tooltip / Popover):推导值是近白 rgba(245,245,247,0.85),而它的文字色是
    // colorTextLightSolid(#fff)→ 白字白底不可读。改成深色面,白字才成立。
    colorBgSpotlight: '#2c2c36',

    // 主文字 / 次文字 —— spec §13 的值。colorTextBase 只是种子,antd 正文实际取 colorText,
    // 两个都写才不会有一处掉回浅色兜底。在 #16161c 上分别是 16.55:1 与 7.03:1,都过 AA。
    colorTextBase: '#f5f5f7',
    colorText: '#f5f5f7',
    colorTextSecondary: '#a1a1aa',
    // 三级文字(占位符、说明文案):neutral 阶梯的第三档,在 #16161c 上 3.73:1 ——
    // 够 3:1(大字号 / 辅助信息),不到正文要求的 4.5:1,所以三级文字不承载正文。
    colorTextTertiary: '#71717a',

    // 圆角 8 / 12(spec §13):8 给控件,12 给面板与卡片。
    borderRadius: 8,
    borderRadiusLG: 12,

    // 强调色:故意不写 colorPrimary —— 让 antd 默认值透出来,与浅色页共用同一套品牌色,
    // 不新造蓝紫(tokens.test.tsx 有断言守着这一条)。
  },
};
