import { useState, type CSSProperties } from 'react';
import { followUser, unfollowUser } from '../../api/follow';
import { setFollowee, useFollowees } from '../../hooks/useFollowees';
import { spacing } from '../../theme/tokens';

// 与 ActionRail / VideoPanel 同一份"浮在视频上的颜色"(见 ActionRail.tsx 顶部):
// 浮层里的颜色各处各写一遍,深浅就不一致了。这里只用已经存在的三个值,不新造第四个。
const TEXT_PRIMARY = '#f5f5f7';
const TEXT_SECONDARY = '#a1a1aa';
const SURFACE = '#2c2c36';

// 宽度按三个字("已关注")给足:两个状态字数不同(2 字 / 3 字),宽度跟着文字走的话,
// 点一下按钮就左右弹一下,看着像页面抖了。48 是 ActionRail 的 SLOT_MIN_SIZE(触摸目标),
// 这里取更宽的 64 是为了同时容下两种文案。
const MIN_WIDTH = 64;
// 44:与 ActionRail 的槽位(48)、FeedList 的"点此重试"(44)、外壳的导航入口(44)、
// VideoPanel 的声音开关(44)同一把尺子 —— 压在视频上的小控件点不中的代价,比"看起来大一点"高。
// (原来是 32。这个高度也是它在右侧栏里的占位高度,与上面那几格的 48 不再差一截。)
const MIN_HEIGHT = 44;

const baseStyle: CSSProperties = {
  display: 'inline-flex',
  alignItems: 'center',
  justifyContent: 'center',
  // border-box:描边 / 内边距不该加在给定尺寸之外(同 ActionRail)
  boxSizing: 'border-box',
  minWidth: MIN_WIDTH,
  minHeight: MIN_HEIGHT,
  padding: `${spacing.xxs}px ${spacing.md}px`,
  border: 'none',
  // 与 antd 的 borderRadius(8,见 theme/tokens.ts)对齐
  borderRadius: 8,
  fontFamily: 'inherit',
  fontSize: 13,
  fontWeight: 600,
  lineHeight: '16px',
};

export interface FollowButtonProps {
  authorId: number;
}

// 关注按钮。
//
// 已关注态从哪来:后端没有"我是否关注 TA"的读接口,只能靠那份全量关注列表
// (useFollowees:模块级缓存、整屏只拉一次)。所以这里**不自己存**"我关注没有",
// 而是每帧问一次缓存 —— 同一个人被别的 rail 关注 / 取关,这一格会立刻跟着变。
export default function FollowButton({ authorId }: FollowButtonProps) {
  const { ids, loading } = useFollowees();
  const following = ids.has(authorId);
  // 请求在飞时挡住第二次点击:方向相反的两次响应谁后回来谁覆盖状态,界面就会随机错。
  const [pending, setPending] = useState(false);

  const handleClick = async () => {
    if (pending || loading) return;
    setPending(true);
    try {
      const resp = following ? await unfollowUser(authorId) : await followUser(authorId);
      // 不乐观更新、也不自己取反:直接采用响应里的 following。关注数有上限,
      // 服务端可能拒绝(那时回的是 false)—— 自己取反就会把"没关注上"显示成"已关注"。
      setFollowee(authorId, resp.following);
    } catch {
      // 失败保持原样。错误提示由 api/axios.ts 的拦截器统一弹,这里再弹一次就是两条 toast。
    } finally {
      setPending(false);
    }
  };

  // 全量列表还没回来时禁用:此刻 ids 还是空集,点下去可能对"本来就关注了"的人再发一次关注
  // (紧接着还要按响应改回来)。文字这一帧仍按"关注"渲染 —— 把按钮藏起来会让右侧栏跳一下,
  // 两害相权:先给一个禁用态,列表一到立刻纠正。
  const disabled = pending || loading;

  return (
    <button
      type="button"
      data-testid="follow-button"
      // 关注与否是个开关状态,交给 aria-pressed 表达;可访问名就是可见文字
      // ("关注" / "已关注"),读屏念的和眼睛看到的不会打架。
      aria-pressed={following}
      disabled={disabled}
      onClick={handleClick}
      style={{
        ...baseStyle,
        // 未关注:浅底深字 —— 右侧栏里唯一一个"要你动手"的控件,得一眼认出来;
        // 已关注:退回深底次色文字,弱化下去,不再抢注意力。
        background: following ? SURFACE : TEXT_PRIMARY,
        color: following ? TEXT_SECONDARY : SURFACE,
        // 浅底压在亮画面上会和画面糊在一起,补一层与遮罩同源(#0b0b0f)的暗影 ——
        // 不新造颜色,和 ActionRail 计数用的文字阴影是同一个来源。
        boxShadow: following ? 'none' : '0 1px 3px rgba(11, 11, 15, 0.45)',
        opacity: disabled ? 0.6 : 1,
        cursor: disabled ? 'default' : 'pointer',
      }}
    >
      {following ? '已关注' : '关注'}
    </button>
  );
}
