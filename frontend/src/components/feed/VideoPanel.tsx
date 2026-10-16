import { useEffect, useRef, useState, type CSSProperties, type ReactNode } from 'react';
import { Typography } from 'antd';
import { useInView } from '../../hooks/useInView';
import {
  FULL_VIEWPORT_HEIGHT,
  RAIL_AVOID_WIDTH,
  SCRIM_MIN_HEIGHT,
  SIDEBAR_WIDTH,
  palette,
  spacing,
} from '../../theme/tokens';
import type { FeedItem } from '../../types/feed';

// 浮在视频上的文字色取自 theme/tokens.ts 的 palette(唯一定义)——
// 不从这个子树里的 ConfigProvider 读:浮层压的是画面,必须自己保证可读,
// 不能依赖外层恰好套了深色(一旦它落在浅色子树里,antd 会给近黑文字压在暗遮罩上,等于白写)。
const TEXT_PRIMARY = palette.textPrimary;
const TEXT_SECONDARY = palette.textSecondary;

// 与 ActionRail 的计数、FollowButton 的浅底按钮同一个来源(palette.background):压在视频上的文字
// 不能只赌"这一段遮罩恰好够暗"。画面亮起来时,一层与遮罩同源的暗影把笔画边缘压回可读 ——
// 不新造颜色,与浮层里的其它文字同一套做法。
const TEXT_SHADOW = '0 1px 2px rgba(11, 11, 15, 0.9)';

// 声音开关的触摸目标:44×44。圆形视觉只有 40px 时,手指是点不中的(与 ActionRail 的 48、
// FeedList 的"点此重试" 44、外壳入口的 44 同一把尺子)。
const MUTE_BUTTON_SIZE = 44;

/** 播放失败的分诊:**只有这一份**。两条入口(视口驱动的那次 play、点画面重试的那次 play)
 *  必须按同一套规矩说话 —— 原来重试那条是 `catch(() => {})`,与这里的分类自相矛盾:
 *  同一个失败,一个静默、一个报错,取决于它是谁触发的。
 *
 *  为什么必须分两类:
 *    NotAllowedError —— 自动播放策略拦下(用户还没交互),预期内,静默;
 *    AbortError       —— 切源 / 暂停把这次 play 打断了,也预期内;
 *    其余(NotSupportedError / 解码失败 / 源 404)说明这一屏根本播不了 ——
 *    那时用户看到的是一张永远不动的封面,不报出来就永远查不到。 */
function reportPlayFailure(err: unknown): void {
  const name = err instanceof DOMException ? err.name : '';
  if (name !== 'NotAllowedError' && name !== 'AbortError') {
    console.error('[VideoPanel] play 失败', err);
  }
}

/** 长文案的截断:最多 lines 行,超出给省略号。
 *
 *  为什么必须截:左下角这块文字是**从底边往上长**的,越长越往上顶 —— 顶过遮罩那段渐变
 *  就变成"亮字压亮画面"(矮视口 + 长描述尤其明显)。给了上限,这块的高度就是有界的。
 *  为什么是 -webkit-box + line-clamp:这是目前唯一能"截断并给省略号"的做法;
 *  max-height + overflow:hidden 会把一行字从中间切掉,连个提示都没有。 */
const clampLines = (lines: number): CSSProperties => ({
  display: '-webkit-box',
  WebkitBoxOrient: 'vertical',
  WebkitLineClamp: lines,
  overflow: 'hidden',
});

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

  // 切声音。两条入口:点画面(鼠标习惯)和右上角那个真 <button>(键盘 / 读屏)——
  // 整屏 div 挂 onClick 对键盘用户不可达,所以语义化控件那条不能省。
  const toggleMute = () => {
    const el = videoRef.current;
    setMuted((m) => !m);
    // 这次点击是真实的用户手势:若这一屏正卡在自动播放策略上(NotAllowedError),
    // 借这个手势把它捞回来,用户就不会只看到一张永远不动的封面。
    // 失败交给 reportPlayFailure:与下面的自动播放同一条分诊规矩(真播不了要现形),
    // 不再写一个吞掉一切的空 catch —— 那样同一个失败在两处会有两种待遇。
    if (el?.paused) {
      el.play()?.catch(reportPlayFailure);
    }
  };

  // 播放权的唯一开关:在视口里就播,不在就停。
  // 放在 effect 里而不是渲染期直接调,是因为 <video> 是命令式的:渲染只声明它长什么样,
  // play / pause 是副作用。依赖只有 inView,滚动时不会每次重渲染都重新调 play。
  useEffect(() => {
    const el = videoRef.current;
    if (!el) return;

    if (inView) {
      // muted 默认 true,所以这里不用管"带声音自动播放会被拦"。
      // 用可选链是因为测试环境(jsdom)没实现媒体播放,根本不返回 Promise。
      // 失败的分诊在 reportPlayFailure 一处:它也是"点画面重试"那条入口用的同一个函数。
      el.play()?.catch(reportPlayFailure);
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
      // doc 里"点一下切声音":点画面任意处都算(鼠标习惯)。键盘 / 读屏走右上角那个真按钮。
      onClick={toggleMute}
      style={{
        position: 'relative',
        // 自己撑满一屏,不依赖父容器高度:父容器若是 auto 高,height:100% 会算成 0,
        // 视频整块塌掉。滚动的吸附(scroll-snap)由外层的列表容器管,不归这里。
        //
        // 高度与外壳 / FeedList 的根**同一个定义**(theme/tokens.ts 的 FULL_VIEWPORT_HEIGHT):
        // 三者必须相等,差一条就意味着面板和它所在的那一屏错开。
        // 100vh 是"大视口"高 —— 移动端带地址栏时它比真正看得见的高度大,底部那排会被压到
        // 地址栏下面;100dvh 跟着地址栏伸缩,不支持 dvh 的老浏览器忽略 maxHeight 退回 100vh。
        ...FULL_VIEWPORT_HEIGHT,
        width: '100%',
        overflow: 'hidden',
        // 首帧解出来之前 / 加载失败时的底色,别让画面露白(与遮罩同源的沉浸底)
        background: palette.background,
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

      {/* 声音开关:键盘与读屏用户唯一的可达入口(整屏 onClick 对他们不可达),
          所以是个真 <button>,不是一个可点的 div。 */}
      <button
        type="button"
        aria-label="视频声音"
        aria-pressed={!muted}
        onClick={(e) => {
          e.stopPropagation();
          toggleMute();
        }}
        style={{
          position: 'absolute',
          right: spacing.md,
          top: spacing.md,
          width: MUTE_BUTTON_SIZE,
          height: MUTE_BUTTON_SIZE,
          borderRadius: 999,
          border: 'none',
          cursor: 'pointer',
          // 圆形底:沉浸底(palette.background = #0b0b0f)的 60% 透明版 —— 压在白画面上也读得清图标
          background: 'rgba(11, 11, 15, 0.6)',
          color: TEXT_PRIMARY,
          fontSize: 18,
          lineHeight: 1,
        }}
      >
        {muted ? '🔇' : '🔊'}
      </button>

      {/* 底部整条渐变遮罩:从沉浸底(palette.background)渐变到透明。
          文字和右侧操作栏都落在这条带上,所以它是"浮层可读"的地基 —— 画面亮起来时
          白字直接压上去是读不清的。
          另外两道保险(都在下面这块文字自己身上):与遮罩同源的文字暗影,以及标题 / 描述的
          限行 —— 这块文字是从底边往上长的,限了行,它的高度才有界,不会顶到渐变很淡的那一段。 */}
      <div
        data-testid="video-panel-scrim"
        style={{
          position: 'absolute',
          left: 0,
          right: 0,
          bottom: 0,
          // 百分比高度会随视口被压扁:矮视口(手机横屏 / 矮窗口)下文字会滑到渐变很淡的那段上,
          // 白字压亮画面实测 ≈2.9:1,低于 3:1 底线。给一个像素下限(值和它的理由在 tokens 里),
          // 保证文字始终落在够暗的段上。
          height: SCRIM_MIN_HEIGHT,
          // 渐变从沉浸底(palette.background)到透明:暗端的 hex 不在这里再写一遍,
          // 后面两段是同一个色的透明度变体,所以用 rgb 字面量。
          backgroundImage: `linear-gradient(to top, ${palette.background} 0%, rgba(11, 11, 15, 0.72) 45%, rgba(11, 11, 15, 0) 100%)`,
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
          // 两侧都要避让浮层:右边是操作栏(RAIL_AVOID_WIDTH,算式在 tokens 里),
          // 左边是 MainLayout 那条 fixed 导航(SIDEBAR_WIDTH)—— 满屏出血下它盖在视频上,
          // 不避让就把作者名压住了。写成 longhand 是为了这两处各有出处、不会互相带跑。
          paddingTop: spacing.md,
          paddingRight: RAIL_AVOID_WIDTH,
          paddingBottom: spacing.xl,
          paddingLeft: SIDEBAR_WIDTH + spacing.md,
          display: 'flex',
          flexDirection: 'column',
          gap: spacing.xxs,
          // 长串(没有空格的地址 / 连续英文)不该顶破这块 —— 中文自己会换行,这条是给它们兜底的。
          overflowWrap: 'anywhere',
        }}
      >
        <Typography.Text strong style={{ ...clampLines(1), color: TEXT_PRIMARY, fontSize: 16, textShadow: TEXT_SHADOW }}>
          @{video.username}
        </Typography.Text>
        {/* 标题两行 / 描述三行封顶:这块文字从底边往上长,不限行就会顶到遮罩变淡的那一段上
            (矮视口 + 长描述尤其明显),白字压亮画面等于读不清。 */}
        <Typography.Text style={{ ...clampLines(2), color: TEXT_PRIMARY, fontSize: 14, textShadow: TEXT_SHADOW }}>
          {video.title}
        </Typography.Text>
        {video.description && (
          <Typography.Text style={{ ...clampLines(3), color: TEXT_SECONDARY, fontSize: 13, textShadow: TEXT_SHADOW }}>
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
