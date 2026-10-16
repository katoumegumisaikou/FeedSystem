import { useEffect, useState, type CSSProperties } from 'react';
import { Link } from 'react-router-dom';
import { likeVideo, unlikeVideo } from '../../api/video';
import { palette, spacing } from '../../theme/tokens';
import type { FeedItem } from '../../types/feed';
import FollowButton from './FollowButton';
import { CommentIcon, HeartIcon, UserIcon } from './icons';

// 与 VideoPanel 同一份"浮在视频上的文字色" —— 取自 theme/tokens.ts 的 palette(唯一定义)。
// 不新造颜色:浮层里的颜色各处自己写一遍,深浅就不一致了(而且这些字压在画面遮罩上,
// 自造一个更亮的白只会让对比度失控)。
const TEXT_PRIMARY = palette.textPrimary;
const TEXT_SECONDARY = palette.textSecondary;

// 计数是浮在视频上的数字:遮罩只保证"底部那一段够暗",画面亮起来时纯白字仍然会糊。
// 再压一层与遮罩同源(palette.background)的文字阴影 —— 等于把可读性从"画面恰好够暗"这个赌注上拿回来。
const COUNT_TEXT_SHADOW = '0 1px 2px rgba(11, 11, 15, 0.9)';

const ICON_SIZE = 28;
const AVATAR_SIZE = 48;
// 触摸目标的最小方块:图标本身才 28px,不留够可点面积,手指就会"点不中"。
const SLOT_MIN_SIZE = 48;

const slotStyle: CSSProperties = {
  display: 'flex',
  flexDirection: 'column',
  alignItems: 'center',
  justifyContent: 'center',
  gap: spacing.xxs,
  minWidth: SLOT_MIN_SIZE,
  minHeight: SLOT_MIN_SIZE,
};

const countTextStyle: CSSProperties = {
  color: TEXT_PRIMARY,
  fontSize: 13,
  fontWeight: 600,
  lineHeight: '16px',
  textShadow: COUNT_TEXT_SHADOW,
};

export interface ActionRailProps {
  video: FeedItem;
}

// 右侧操作栏:头像 / 点赞 / 评论 / 关注,自上而下。
//
// 它只管"这一条视频的互动入口长什么样",不管自己浮在哪里 —— 定位(右下角、浮在遮罩带上、
// 拦住点击免得冒泡到切声音)由 VideoPanel 的右侧槽位负责,组合点在 Task 8 的 FeedList。
// 两边都不需要知道对方存在,所以本组件既不读视口,也不套遮罩。
export default function ActionRail({ video }: ActionRailProps) {
  // 本地状态只是"这一条现在的样子":初值取传来的本条视频,之后由接口返回的真值纠正。
  // 后端 LikeResp 同时回 is_like 与 likes_count(而不是只回 bool),就是为了让前端不必自己 ±1 ——
  // 并发点赞下那个数只有服务端算得准。
  const [isLiked, setIsLiked] = useState(Boolean(video.is_like));
  const [likesCount, setLikesCount] = useState(video.likes_count);
  // 请求在飞时把点击挡住:连点两下会发出两个方向相反的请求,谁后回来谁覆盖状态,
  // 计数就会随机错。宁可第二次点击被忽略,也不要一个可能错的数字。
  const [pending, setPending] = useState(false);

  // props 变了就跟着重置(props 是本条视频当前的真值)。这条同时兜住一种情况:
  // 调用方没按 video.id 加 key,同一个位置直接换成了下一条视频 —— 否则上一条的点赞态会留在屏幕上。
  // props 没变时这个 effect 不会跑,也就不会把刚纠正过的状态又冲掉。
  useEffect(() => {
    setIsLiked(Boolean(video.is_like));
    setLikesCount(video.likes_count);
  }, [video.id, video.is_like, video.likes_count]);

  const handleLike = async () => {
    if (pending) return;
    setPending(true);
    try {
      const resp = isLiked ? await unlikeVideo(video.id) : await likeVideo(video.id);
      // 不乐观更新、也不本地加减:直接采用响应里的两个字段。这样失败路径天然无需回滚 ——
      // 没拿到响应就什么都没改。
      setIsLiked(resp.is_like);
      setLikesCount(resp.likes_count);
    } catch {
      // 失败保持原样。错误提示由 api/axios.ts 的拦截器统一弹出,这里再弹一次就是两条 toast。
    } finally {
      setPending(false);
    }
  };

  return (
    <div
      data-testid="action-rail"
      style={{
        display: 'flex',
        flexDirection: 'column',
        alignItems: 'center',
        gap: spacing.md,
      }}
    >
      {/* 1. 头像 → 作者主页。用 <Link> 而不是"按钮 + navigate":跳转本来就该由链接承担 ——
          读屏念的是"链接",键盘可聚焦,还能右键在新标签页打开。 */}
      <Link
        to={`/users/${video.author_id}`}
        data-testid="action-rail-avatar"
        aria-label={`查看 ${video.username} 的个人主页`}
        style={{
          display: 'block',
          width: AVATAR_SIZE,
          height: AVATAR_SIZE,
          // border-box:不然 2px 描边会加在 48 之外,圆圈跟下面几格对不齐
          boxSizing: 'border-box',
          borderRadius: '50%',
          overflow: 'hidden',
          border: `2px solid ${TEXT_PRIMARY}`,
          background: palette.border,
        }}
      >
        {video.avatar_url ? (
          // alt 留空:这个 <img> 是链接的装饰,链接已经有名字(aria-label),
          // 再给 alt 只会让读屏把作者名念两遍。
          <img
            src={video.avatar_url}
            alt=""
            style={{ display: 'block', width: '100%', height: '100%', objectFit: 'cover' }}
          />
        ) : (
          // avatar_url 在 FeedItem 里是可选的:给个占位人像,而不是一张破图
          <span
            style={{
              display: 'flex',
              alignItems: 'center',
              justifyContent: 'center',
              width: '100%',
              height: '100%',
              color: TEXT_SECONDARY,
            }}
          >
            <UserIcon size={24} />
          </span>
        )}
      </Link>

      {/* 2. 点赞 → 真接口。aria-pressed 表达"赞没赞"这个开关状态;
          名字里带上当前计数 —— aria-label 一存在,里面那个数字就不会被单独念出来。
          aria-busy 表达"这一下正在处理":请求在飞时连点会被忽略(见上面的 pending),
          视觉上也只降一点不透明度 —— 读屏至少要知道事情在走,而不是一按什么都没发生。 */}
      <button
        type="button"
        data-testid="action-rail-like"
        aria-pressed={isLiked}
        aria-busy={pending}
        aria-label={`${isLiked ? '取消点赞' : '点赞'}，当前 ${likesCount} 个赞`}
        onClick={handleLike}
        style={{
          ...slotStyle,
          padding: 0,
          border: 'none',
          background: 'none',
          cursor: 'pointer',
          // 在飞时淡一档:与 FollowButton 的禁用态同一个手法(不新造颜色),读屏那边由 aria-busy 兜住
          opacity: pending ? 0.7 : 1,
          // 图标用 currentColor → 与它下面的数字同色,不用给图标再指定一次颜色
          color: TEXT_PRIMARY,
        }}
      >
        <HeartIcon size={ICON_SIZE} filled={isLiked} />
        <span style={countTextStyle}>{likesCount}</span>
      </button>

      {/* 3. 评论:只有图标 + 数字。后端还没有评论模块,所以这里**故意不是控件** ——
          一个点了什么都不发生的 <button> 对键盘 / 读屏用户是纯陷阱(能聚焦,却没有任何作用)。
          role="img" + 名字让读屏听到"评论 8",而不是一个孤零零的数字。 */}
      <div
        data-testid="action-rail-comment"
        role="img"
        aria-label={`评论 ${video.comment_count}`}
        style={{ ...slotStyle, color: TEXT_PRIMARY }}
      >
        <CommentIcon size={ICON_SIZE} />
        <span style={countTextStyle}>{video.comment_count}</span>
      </div>

      {/* 4. 关注 → FollowButton。它自己管"已关注没有"(那份全量关注列表缓存在 useFollowees
          里,模块级、整屏一份),ActionRail 只把本条视频的 author_id 递进去 ——
          右侧栏不必知道关注接口长什么样,也不必知道列表缓存在哪。 */}
      <div data-testid="action-rail-follow">
        <FollowButton authorId={video.author_id} />
      </div>
    </div>
  );
}
