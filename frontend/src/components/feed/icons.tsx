// 内联 SVG 图标。
//
// 为什么不引图标库:项目刻意没有图标依赖(package.json 里一个都没有,这是有意的)——
// 整个应用只用到三四个图标,为它们拉一整套图标包(及其主题 / 按需加载配置)不划算;
// 而且这里画的是"浮在视频上的深色控件",用 currentColor 的内联 SVG 才能跟着外层文字色走,
// 不必再为深色背景补一套图标主题。
//
// 约定:
//   - viewBox 统一 0 0 24 24,尺寸由 size 给 —— 调用方需要多大画多大,不做多套尺寸档位
//   - 颜色一律 currentColor:图标永远和它旁边那个数字同色,不给图标单独造色
//   - 默认 aria-hidden:图标本身没有信息量,语义由外面那个控件(或 role="img" 的格子)提供 ——
//     让读屏再念一遍"心形"只会把按钮名字念乱

export interface IconProps {
  /** 边长(px),默认 24。 */
  size?: number;
}

/** 点赞的心形。filled=true 画实心(已点赞),false 画空心。 */
export function HeartIcon({ size = 24, filled = false }: IconProps & { filled?: boolean }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      aria-hidden="true"
      focusable="false"
      // 已点赞 / 未点赞的差别只靠"填满 / 不填满"表达,不新造一个 token 外的红色 ——
      // 浮层上的颜色各写各的,深浅就会不一致(状态本身另有 aria-pressed 兜底)。
      fill={filled ? 'currentColor' : 'none'}
      stroke={filled ? 'none' : 'currentColor'}
      strokeWidth={1.8}
      strokeLinejoin="round"
    >
      <path d="M12 21.35l-1.45-1.32C5.4 15.36 2 12.28 2 8.5 2 5.42 4.42 3 7.5 3c1.74 0 3.41.81 4.5 2.09C13.09 3.81 14.76 3 16.5 3 19.58 3 22 5.42 22 8.5c0 3.78-3.4 6.86-8.55 11.54L12 21.35z" />
    </svg>
  );
}

/** 评论的气泡。 */
export function CommentIcon({ size = 24 }: IconProps) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      aria-hidden="true"
      focusable="false"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.8}
      strokeLinejoin="round"
    >
      {/* 气泡本体:椭圆比圆角矩形好画,也不会因为圆角半径写错而变形 */}
      <ellipse cx="12" cy="10.8" rx="8.8" ry="6.6" />
      {/* 小尾巴:从气泡左下边缘往下拉出一个尖,否则只是个椭圆看不出是"说话" */}
      <path d="M7.6 16.6 6 20.4l4.6-2.7" />
    </svg>
  );
}

/** 人像,用在没有头像图时的占位圆里。 */
export function UserIcon({ size = 24 }: IconProps) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      aria-hidden="true"
      focusable="false"
      fill="currentColor"
    >
      <circle cx="12" cy="8.4" r="3.9" />
      <path d="M12 13.4c-3.7 0-6.7 2.3-6.7 4.6 0 .7.5 1.2 1.2 1.2h11c.7 0 1.2-.5 1.2-1.2 0-2.3-3-4.6-6.7-4.6z" />
    </svg>
  );
}
