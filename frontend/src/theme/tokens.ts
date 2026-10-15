import type { ThemeConfig } from 'antd';

// 深色设计 token —— spec §13「深色 token 起始值」那张表落成的 antd 形态。
//
// 为什么是一份独立的 ThemeConfig、而不是改 main.tsx 里的全局 ConfigProvider:
// 深色只属于新的 feed 页面(登录 / 注册 / 个人主页保持浅色),所以由 Task 9 的 MainLayout
// 在自己的子树里嵌套 <ConfigProvider theme={darkTokens}>。嵌套只覆盖这里显式写出的 token,
// 父层那些浅色页拿不到任何一项,也就不会被刷黑。
//
// 为什么只写 token、不挂 darkAlgorithm:算法是全局的,挂上会顺着 ConfigProvider 树影响
// 所有后代;而且这几个色值来自设计稿,不是算法推导的结果。代价是这里没显式写的 token 仍旧
// 按「浅色算法 + 深色种子」推:colorFill* / colorTextQuaternary 推出来还能用,描边类不行 ——
// antd 推导描边用的是 getSolidColor(把底色**压暗**),而 #0b0b0f 已经接近纯黑,推导值全是
// #000000(实测):所以 colorBorder 必须显式给(见下);colorBorderSecondary 同理(推导值
// 也是纯黑),它管的是分隔线,等 Task 9 / 11 真有分隔线要画时按需补。

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

export const darkTokens: ThemeConfig = {
  token: {
    // 背景(沉浸底)与表面(侧栏 / 面板)—— spec §13 的两个底色。
    colorBgBase: '#0b0b0f',
    colorBgContainer: '#16161c',
    // 浮层(下拉、弹层、气泡)要比面板再亮一档,否则弹层与面板糊在一起分不出层级。
    colorBgElevated: '#1c1c24',
    // 描边:必须显式给 —— 见文件头,推导值是纯黑 #000000,等于没有描边。
    colorBorder: '#2c2c36',

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
