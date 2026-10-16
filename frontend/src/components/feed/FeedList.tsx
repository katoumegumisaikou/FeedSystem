import { useCallback, useEffect, useRef, useState, type CSSProperties } from 'react';
import { useInView } from '../../hooks/useInView';
import { spacing } from '../../theme/tokens';
import type { FeedItem } from '../../types/feed';
import ActionRail from './ActionRail';
import VideoPanel from './VideoPanel';

// 四态文案 —— spec §13 那张表的原话,逐字搬过来(不在这里改写文案)。
const LOADING_TEXT = '加载中…';
const ERROR_TEXT = '加载失败,点此重试';
const END_TEXT = '没有更多了';

// 与 VideoPanel / ActionRail / FollowButton 同一份深色浮层配色(见 ActionRail.tsx 顶部):
// 浮层里的颜色各处各写一遍,深浅就不一致了 —— 这里只用已经存在的四个值,不新造第五个。
const TEXT_PRIMARY = '#f5f5f7';
const TEXT_SECONDARY = '#a1a1aa';
const SURFACE = '#16161c';
const BORDER = '#2c2c36';
const IMMERSIVE_BG = '#0b0b0f';

// 「到底」的唯一判据:**游标缺失**。
//
// 为什么不能看 items.length:一页比 limit 短、甚至一条都没有,都是合法情况 ——
// 本页候选被截断、窗口内条目已失效(视频下架 / 作者取关)时,游标之后确实还有内容。
// 只看长度会提前判到底,把后面的内容整段漏掉。
//
// 为什么用 `== null`(而不是 `=== undefined` 或 `!cursor`):
//   · 后端到底时 next_cursor 这个键**整个不发出来**(Go 的 *FeedCursor + omitempty),
//     前端拿到的是 undefined —— 这是契约里那一种;
//   · null 是保险:万一哪天后端把它写成显式的 null,`load(null)` 会被无限跟随
//     (每页都回同一个 null),那就是死循环;
//   · 不用 `!cursor`:游标是泛型,可能是 `0` / `''` 这类合法的假值,`!` 会误判成到底。
function isExhausted<C>(cursor: C | undefined): boolean {
  return cursor == null;
}

export interface FeedListProps<C> {
  load: (cursor?: C) => Promise<{ items: FeedItem[]; nextCursor?: C }>;
  emptyText: string;
}

// 三个视频流共用的容器:无限加载(游标分页)+ 加载中 / 空 / 错误(可重试)/ 到底 四态。
//
// 为什么泛型:三个流的游标形状不同(latest / following 是 (created_at, video_id),
// like 是 (likes_count, video_id)),共用一个函数就会拼错参数。这里把游标当成一个
// **不透明**的值:容器只负责"原样收进来、原样递回去",是什么形状由页面自己的适配器决定
// (适配器同时把 API 的 next_cursor 改名成 nextCursor)。所以这个文件不 import 任何 feed api。
export default function FeedList<C>({ load, emptyText }: FeedListProps<C>): JSX.Element {
  const [items, setItems] = useState<FeedItem[]>([]);
  // 下一页要带上的游标(不透明,容器不解读它)。
  const [cursor, setCursor] = useState<C | undefined>(undefined);
  // 初值 true:第一帧还没发请求,不能当成"空" —— 空态的含义是"拉完了,确实没有"。
  // 否则首屏会先闪一下"还没有视频",再换成视频,那是假的。
  const [loading, setLoading] = useState(true);
  // 只存"失败过"这个事实,不存 Error 对象:界面只需要一个重试入口,错误提示由
  // api/axios.ts 的拦截器统一弹,这里再弹一次就是两条 toast。
  const [failed, setFailed] = useState(false);
  // 到底(上游把游标省略了)。**只有它和 items 一起决定"没有更多了"**。
  const [done, setDone] = useState(false);

  // 底部哨兵:它只回答"用户碰到列表尾部了吗"这一个问题。
  // 阈值给 0 而不是 useInView 的默认 0.6:哨兵是 1px 高的一条,压着一两个像素就该算触底 ——
  // 0.6 要求它六成进视口,而满屏吸附(scroll-snap)下,滑到最后一屏时它正好卡在视口边界上,
  // 那就永远等不到"触底"。0.6 的语义属于 VideoPanel("六成进画面才算在看它"),不是这里。
  const [sentinelRef, sentinelInView] = useInView<HTMLDivElement>({ threshold: 0 });

  // 并发保护的第一道闸:在飞的请求没回来,谁都不许再发。
  // 用 ref 而不是读 loading 这个 state:state 的更新是异步的,同一帧里连续两次触发
  // (哨兵回调 + 用户点重试)会都看到 loading=false,于是双发。
  // 第二道闸是下面 effect 里的 loading / failed / done 三个条件。
  const inFlight = useRef(false);

  const loadPage = useCallback(
    async (next?: C) => {
      if (inFlight.current) return;
      inFlight.current = true;
      setLoading(true);
      try {
        const page = await load(next);
        setItems((prev) => [...prev, ...page.items]);
        // 「到底」只在这一处判定:游标缺失。绝不看 page.items.length(见 isExhausted 上面的注释)。
        setCursor(page.nextCursor);
        setDone(isExhausted(page.nextCursor));
        // 成功即清掉失败态 —— 重试成功的路径也要能把"加载失败"抹掉。
        setFailed(false);
      } catch {
        // 失败只记一个事实,并且**保留当前游标**:重试要用同一个游标把这一页再拉一次,
        // 失败就把游标推走的话,那一页会被整段跳过。
        // 也必须把失败这个事实记下来 —— 否则下面触底那个 effect 会在"没有在加载"
        // 这个状态下立刻再触发一次,坏掉的接口就被打成重试风暴。
        setFailed(true);
      } finally {
        // 先放开闸门、再落 loading:两条都在同一次批处理里生效,中间不会出现
        // "已经不在加载了、闸门却还关着"这种会漏掉一次触发的窗口。
        inFlight.current = false;
        setLoading(false);
      }
    },
    // load 来自页面适配器(不是内联箭头就是模块级函数),引用变了只是让 effect 重跑一次,
    // 上面三道闸会拦住任何多余请求。
    [load],
  );

  // 第一页:挂载即拉,不等哨兵。
  // 为什么不挂在哨兵上:①「第一页」跟"滚到哪"无关;② 哨兵依赖 IntersectionObserver,
  // 它不存在的环境里(useInView 会一直返回不可见)首屏会永远停在"加载中…"。
  // 依赖数组故意为空:一屏流只在挂载时拉一次首屏,切页面时整棵树重建。
  useEffect(() => {
    void loadPage(undefined);
  }, []);

  // 触底:哨兵进视口就带着当前游标拉下一页。三道闸缺一不可 ——
  //   ① loading:请求在飞,不许再发(并发保护);
  //   ② failed:失败必须由用户点"重试",自动重发会把一个坏掉的接口打成重试风暴;
  //   ③ done:到底了(游标缺失),再发只会把同一页要回来。
  // cursor 在依赖里是有意的:上一页若是空的(或短页)但游标还在,这一条会让容器**继续**跟着游标走,
  // 而不是因为"这一页没东西"就停住 —— 这正是本组件存在要解决的那个坑。
  useEffect(() => {
    if (!sentinelInView) return;
    if (loading || failed || done) return;
    void loadPage(cursor);
  }, [sentinelInView, loading, failed, done, cursor, loadPage]);

  // 三态归一。优先级:加载中 > 失败 > 到底 —— 重试在飞的时候应该显示"加载中…",
  // 而不是继续摆着"加载失败"(那条提示此刻已经过期了)。
  const empty = items.length === 0;
  const state: 'loading' | 'error' | 'end' | null = loading
    ? 'loading'
    : failed
      ? 'error'
      : done
        ? 'end'
        : // 一条都没有、也还没到底:屏幕上此刻什么都没有,而容器确实还在往下一段走
          // (等哨兵把下一页拉起来)。这一帧显示"加载中…"比留一片黑诚实 ——
          // 首屏那一帧,以及 IntersectionObserver 不可用的环境,都会落到这里。
          empty
          ? 'loading'
          : null;

  const stateContent =
    state === 'loading' ? (
      LOADING_TEXT
    ) : state === 'error' ? (
      // 整条文案就是按钮:点哪儿都能重试,不用去瞄那几个字。
      <button type="button" data-testid="feed-list-retry" onClick={() => void loadPage(cursor)} style={retryStyle}>
        {ERROR_TEXT}
      </button>
    ) : empty ? (
      // 一条都没有而且确实没有下一页 —— 这才是空态。emptyText 由三个页面各自给
      // (spec:主页"还没有视频 · 去上传你的第一条" / 关注流"你还没有关注的人 · 去主页看看")。
      emptyText
    ) : (
      END_TEXT
    );

  return (
    <div data-testid="feed-list-root" style={rootStyle}>
      {/* 滚动容器:一屏一个面板,y 轴吸附。吸附权归这里 —— VideoPanel 的注释把 scroll-snap
          明确留给了外层列表容器(它自己只管撑满一屏 + 撑多高)。 */}
      <div data-testid="feed-list" style={scrollerStyle}>
        {items.map((item) => (
          // key 用视频 id(稳定),不用下标 —— 追加 / 删除时下标会让 React 错认元素,
          // 上一条的点赞态就会留在新的一条上。
          //
          // 包装元素只负责"这一屏是可吸附的一屏":VideoPanel 不接受 className / style,
          // 而吸附值必须写在它外面(它自己的 100vh / 100dvh 是自带的,不用这里再给高度)。
          //
          // ActionRail 从 VideoPanel 的右侧浮层槽位传进去 —— 三块拼图的组合点就在这几行里:
          // VideoPanel 不认识 ActionRail,ActionRail 也不认识列表,两边都不必反向依赖。
          <div key={item.id} data-testid="feed-list-item" style={ITEM_SNAP_STYLE}>
            <VideoPanel video={item}>
              <ActionRail video={item} />
            </VideoPanel>
          </div>
        ))}

        {/* 底部哨兵:1px 的空元素,自己不带任何文案(文案在外层状态浮层里)。
            它必须**一直存在**:useInView 只在挂载时读一次 ref.current,哨兵若晚一帧才出现
            (比如"先显示加载中、有数据了再渲染列表"),这个容器就永远拿不到"触底"这件事 ——
            第一页若是空的,后面的页就再也拉不动了。
            这也解释了为什么四态是**浮层**而不是替换掉整棵列表树。 */}
        <div ref={sentinelRef} data-testid="feed-list-sentinel" style={SENTINEL_STYLE} />
      </div>

      {/* 状态浮层:挂在滚动容器**外面**,所以它钉在视口上,不随内容滚走。
          为什么不放进流里(列表末尾那一块):外层是 y 轴 mandatory 吸附,流里那点高度
          浏览器永远停不住(一松手就弹回最近的一屏),于是"重试"会变成一个点不到的按钮、
          "没有更多了"永远看不见。 */}
      {state !== null && (
        <div
          data-testid="feed-list-status"
          // 加载中 / 到底 是对读屏的状态播报(role=status 隐含 aria-live="polite");
          // 错误态里是一个真按钮,由按钮自己说话,别让容器再报一遍。
          role={state === 'error' ? undefined : 'status'}
          style={empty ? fullScreenStatusStyle : tailStatusStyle}
        >
          {stateContent}
        </div>
      )}
    </div>
  );
}

// 沉浸底 + 自己撑满一屏:不依赖父容器高度(父容器若是 auto 高,height:100% 会算成 0,
// 整块塌掉 —— 与 VideoPanel 同一条理由、同一套口径,两处必须是同一个值)。
// 100dvh 跟着移动端地址栏伸缩,老浏览器忽略 maxHeight 就退回 100vh。
const rootStyle: CSSProperties = {
  position: 'relative',
  height: '100vh',
  maxHeight: '100dvh',
  width: '100%',
  // 浮层要钉在视口上而不是被内容顶开,所以溢出在这里截断,滚动交给里面那层
  overflow: 'hidden',
  background: IMMERSIVE_BG,
};

const scrollerStyle: CSSProperties = {
  height: '100%',
  overflowY: 'auto',
  // 滑一下正好落在下一个面板的顶边(抖音式)。不用 proximity:满屏流要的是"一定对齐",
  // proximity 在"差得远"时会放弃吸附,滑完停在两屏中间就是画面被切成两半。
  scrollSnapType: 'y mandatory',
  // 滚到底再往上拽别把外层(整页)一起带走:这里就是滚动的终点。
  overscrollBehaviorY: 'contain',
};

// 每一屏的吸附点:对齐到滚动容器的顶边。
const ITEM_SNAP_STYLE: CSSProperties = {
  scrollSnapAlign: 'start',
};

// 哨兵:1px 高,不占视觉、不做吸附点(它不是一屏)。
const SENTINEL_STYLE: CSSProperties = {
  height: 1,
};

// "还没有任何内容"时(首屏加载中 / 空 / 首屏失败):状态占满整屏居中 ——
// 此刻屏幕上没有视频,提示就是全部内容。
const fullScreenStatusStyle: CSSProperties = {
  position: 'absolute',
  inset: 0,
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  padding: spacing.xl,
  textAlign: 'center',
  color: TEXT_SECONDARY,
  fontSize: 14,
  // 浮层本身不吃点击(它盖着整屏),但里面的重试按钮自己把 pointer-events 打开。
  pointerEvents: 'none',
};

// 已经有视频在屏上:提示退成底部居中的一小条 —— 不该盖住画面,也不该抢视线。
const tailStatusStyle: CSSProperties = {
  position: 'absolute',
  // 贴到 4px:VideoPanel 左下角的信息块是从底边 36px 起画的(它自己的 padding),
  // 压到 4px 就不会盖在那几行字上;右侧操作栏在右半边,居中的小条也碰不到它。
  bottom: spacing.xxs,
  left: '50%',
  transform: 'translateX(-50%)',
  maxWidth: '80%',
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  minHeight: 28,
  padding: `${spacing.xxs}px ${spacing.md}px`,
  // 深色面 + 描边:它压在视频上,必须自带底色,否则画面亮起来就读不清
  // (与 VideoPanel 遮罩同一个思路;面用 #16161c、描边用 #2c2c36,都是已有值)。
  background: SURFACE,
  border: `1px solid ${BORDER}`,
  borderRadius: 8,
  color: TEXT_SECONDARY,
  fontSize: 13,
  pointerEvents: 'none',
};

const retryStyle: CSSProperties = {
  display: 'inline-flex',
  alignItems: 'center',
  // 触摸目标给到 44(与 ActionRail 48 的 SLOT_MIN_SIZE 同一个理由):文案只有 13px,
  // 不留够可点面积,手指就点不中 —— 而"点此重试"恰恰是失败之后唯一的出路。
  minHeight: 44,
  // 压在视频上的字:主文字色(与 VideoPanel / ActionRail 同一份)
  color: TEXT_PRIMARY,
  fontSize: 13,
  fontWeight: 600,
  fontFamily: 'inherit',
  // 横向不留内边距:整条文案都是可点区,只靠下划线说明"这里能点"
  padding: `${spacing.xxs}px 0`,
  border: 'none',
  background: 'none',
  textDecoration: 'underline',
  cursor: 'pointer',
  // 上面的浮层一律 pointer-events:none,这个真按钮自己打开 —— 它必须点得到。
  pointerEvents: 'auto',
};
