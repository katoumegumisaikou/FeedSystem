import { useEffect, useRef, useState, type ReactNode } from 'react';
import { Typography } from 'antd';
import { useInView } from '../../hooks/useInView';
import { spacing } from '../../theme/tokens';
import type { FeedItem } from '../../types/feed';

// 深色 token 里的文字色,与 theme/tokens.ts 的 colorText / colorTextSecondary 同一个值。
// 这里直接写死而不是从 ConfigProvider 里读:浮在视频上的字必须自己保证可读,
// 不能依赖外层恰好套了深色 --- 一旦它落在浅色子树里,antd 会给近黑文字压在暗遮罩上,等于白写。
const TEXT_PRIMARY = '#f5f5f7';
const TEXT_SECONDARY = '#a1a1aa';

export interface VideoPanelProps {
  video: FeedItem;
  /** 右侧浮层槽位。ActionRail(Task 6)由 Task 8 从这里传进来 ——
   *  本组件不 import 排在后面的任务,只留位置,避免反向依赖。 */
  children?: ReactNode;
}

// 一屏一个视频:满屏出血的 <video> + 浮在画面上的遮罩 / 文字 / 右侧槽位。
//
// 播放权由 useInView 决定的理由见 hooks/useInView.ts:首页会一次性挂上 N 个面板,
// 机器同时播十几个视频会卡,而且只有视口里那一屏才是用户在看的那一屏。
export default function VideoPanel({ video, children }: VideoPanelProps) {
  // 观察的是「这一屏的容器」而不是 <video>:<video> 铺满容器,两者几何完全一致,
  // 但容器不会被条件渲染掉 —— useInView 只在挂载时读一次 ref.current,
  // 那一刻元素若不存在,这个面板就永远拿不到"可见"这个事实。
  const [panelRef, inView] = useInView<HTMLDivElement>();
  const videoRef = useRef<HTMLVideoElement>(null);
  const [muted, setMuted] = useState(true);

  // 播放权的唯一开关:在视口里就播,不在就停。
  // 放在 effect 里而不是渲染期直接调,是因为 <video> 是命令式的:渲染只声明它长什么样,
  // play / pause 是副作用。依赖只有 inView,滚动时不会每次重渲染都重新调 play。
  useEffect(() => {
    const el = videoRef.current;
    if (!el) return;

    if (inView) {
      // muted 默认 true,所以这里不用管"带声音自动播放会被拦"。
      // play() 返回 Promise:用户还没交互过、或元素已被摘掉时会 reject(NotAllowedError /
      // AbortError)。不接住就是一条 unhandled rejection —— 播不动是浏览器的事,
      // 不该把整个首页拖垮。用可选链是因为测试环境(jsdom)没实现媒体播放,根本不返回 Promise。
      el.play()?.catch(() => {});
    } else {
      // 挂载时也不可见 → 这里会顺手 pause 一次:没有 autoPlay 属性时它是空操作,
      // 但万一将来有人加上 autoplay、或浏览器自行恢复播放,这行才是"只有一屏出声"的保证。
      el.pause();
    }
  }, [inView]);

  return (
    <div
      ref={panelRef}
      data-testid="video-panel"
      // doc 里"点一下切声音":点画面任意处都算,所以监听挂在容器上(见下,右侧槽位会拦住自己的点击)
      onClick={() => setMuted((m) => !m)}
      style={{
        position: 'relative',
        // 自己撑满一屏,不依赖父容器高度:父容器若是 auto 高,height:100% 会算成 0,
        // 视频整块塌掉。滚动的吸附(scroll-snap)由外层的列表容器管,不归这里。
        height: '100vh',
        width: '100%',
        overflow: 'hidden',
        // 首帧解出来之前 / 加载失败时的底色,别让画面露白
        background: '#0b0b0f',
      }}
    >
      <video
        ref={videoRef}
        src={video.play_url}
        poster={video.cover_url}
        muted={muted}
        loop
        playsInline
        // 只要 metadata:滑走的面板不该把整份文件拖下来。一页 N 个面板时这条是省流量的关键。
        preload="metadata"
        // 没有 autoPlay,也不会有:自播靠上面的可见性,靠属性就会 N 个面板同时播。
        style={{
          position: 'absolute',
          inset: 0,
          width: '100%',
          height: '100%',
          // 满屏出血:画面铺满可视区(裁剪不可避免,竖屏短视频就是这么处理的)
          objectFit: 'cover',
        }}
      />

      {/* 底部整条渐变遮罩:从沉浸底 #0b0b0f 渐变到透明。
          文字和右侧操作栏都落在这条带上,所以它是"浮层可读"的地基 —— 画面亮起来时
          白字直接压上去是读不清的。 */}
      <div
        data-testid="video-panel-scrim"
        style={{
          position: 'absolute',
          left: 0,
          right: 0,
          bottom: 0,
          height: '38%',
          backgroundImage:
            'linear-gradient(to top, #0b0b0f 0%, rgba(11, 11, 15, 0.72) 45%, rgba(11, 11, 15, 0) 100%)',
          pointerEvents: 'none',
        }}
      />

      {/* 左下角:作者 + 标题 / 描述 */}
      <div
        data-testid="video-panel-info"
        style={{
          position: 'absolute',
          left: 0,
          bottom: 0,
          // 右侧留出操作栏的宽度(36 * 2 + 16 = 88,按 spacing 刻度算),
          // 文字不会伸到点赞按钮底下
          padding: `${spacing.md}px ${spacing.xl * 2 + spacing.md}px ${spacing.xl}px ${spacing.md}px`,
          display: 'flex',
          flexDirection: 'column',
          gap: spacing.xxs,
        }}
      >
        <Typography.Text strong style={{ color: TEXT_PRIMARY, fontSize: 16 }}>
          @{video.username}
        </Typography.Text>
        <Typography.Text style={{ color: TEXT_PRIMARY, fontSize: 14 }}>
          {video.title}
        </Typography.Text>
        {video.description && (
          <Typography.Text style={{ color: TEXT_SECONDARY, fontSize: 13 }}>
            {video.description}
          </Typography.Text>
        )}
      </div>

      {/* 右侧浮层槽位:定位到右下、浮在遮罩带上,并拦住自己的点击 ——
          操作栏里的点赞 / 关注不该冒泡到容器上顺带把声音开关切了。 */}
      {children && (
        <div
          data-testid="video-panel-actions"
          onClick={(e) => e.stopPropagation()}
          style={{
            position: 'absolute',
            right: spacing.md,
            bottom: spacing.xl + spacing.md,
            display: 'flex',
            flexDirection: 'column',
            alignItems: 'center',
            gap: spacing.md,
          }}
        >
          {children}
        </div>
      )}
    </div>
  );
}
