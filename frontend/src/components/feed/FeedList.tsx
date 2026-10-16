import { useCallback, useEffect, useRef, useState, type CSSProperties } from 'react';
import { useInView } from '../../hooks/useInView';
import { FULL_VIEWPORT_HEIGHT, SIDEBAR_WIDTH, spacing } from '../../theme/tokens';
import type { FeedItem } from '../../types/feed';
import ActionRail from './ActionRail';
import VideoPanel from './VideoPanel';

// 四态文案 —— spec §13 那张表的原话,逐字搬过来(不在这里改写文案)。
const LOADING_TEXT = '加载中…';
const ERROR_TEXT = '加载失败,点此重试';
const END_TEXT = '没有更多了';

// 第五句文案,Task 11 新增:**没有 IntersectionObserver 的环境**下手动出口上的那句话。
// 为什么非加不可:那种环境里 useInView 一直回"不可见",哨兵永远不触发 —— 首屏若恰好
// 一条都没有、却还带着游标(合法状态),屏幕上就是一句永远的"加载中…",没有任何出路。
// 一个动词(加载)+ 一个对象(更多),不用"OK / 确定"这类什么都没说的词。
const MORE_TEXT = '加载更多';

// "滚动位置贴到底"的容差(px):scrollTop / clientHeight 在浏览器里会被取整,亚像素滚动、
// 设备像素比 1.25 / 1.5 的屏幕上都会带来 ±1px 的误差;留 2px 才不会把"到底"判丢。
const TAIL_SLACK_PX = 2;

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

  // 这个环境里有没有 IntersectionObserver —— 判据是"渲染这一刻"有没有,所以刻意写在组件里
  // (不是模块顶层的常量):模块顶层只会算一次(导入那一刻),而环境是可以在运行期变的
  // (浏览器扩展、测试替身)。没有它的环境里哨兵永远不会触发,"触底加载"就只剩手动这一条路。
  const canObserve = typeof IntersectionObserver !== 'undefined';

  // 「用户到过列表尾部了吗」—— 只用来决定"没有更多了"那行字什么时候摆出来(见下)。
  // 一旦为真就不再回退:浮层是钉在视口上的,出现又消失地闪一下比晚一点出现难受得多。
  const [reachedTail, setReachedTail] = useState(false);
  // 滚动容器:既是被观察的滚动位置,也是"贴到底"那个判定的读点。
  const scrollerRef = useRef<HTMLDivElement>(null);

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

  // "没有更多了"为什么需要一道门:
  // 状态浮层挂在滚动容器**外面**(见下面那处注释),所以它钉在视口上 —— 这行字一出现就压在
  // **当前这一屏**上。而"到底"完全可能在你还看着前面的内容时就成立(一页 5 条、游标恰好用尽),
  // 于是这行字会一直压在一条你还没看完的视频上。它说的是"后面没有了",不该出现在你还没看的内容上。
  //
  // 门有两条信号,OR 起来(任一条成立就算到过尾部):
  //   ① 哨兵进过视口 —— 与"触底加载"同一个信号。容器里"到达尾部"只有一个概念,不新造第二个。
  //   ② 滚动位置已经贴到底 —— 为什么非有它不可:哨兵是**列表末尾之后**那 1px,而每一屏都正好
  //      一屏高、吸附又是对齐到顶边的,滚到底时吸附会把位置拉回最后一屏的顶边,哨兵正好卡在
  //      滚动区下沿**之外**,只有手指还在拖、位置还没被拉回的那一瞬它才真的进视口。
  //      只靠 ①,这行字可能永远不出现(单条一页时更是连 1px 都拖不动)。
  // ① 只认"从不可见变成可见"那**一次跳变**,而且要求此刻**已经有内容**:
  //   - 有内容:首帧 items 为空时哨兵是滚动区里唯一的孩子,浏览器那次初始 IO 回调会立刻报可见;
  //     若此刻就把门打开,这道门等于不存在(首屏"空页 + 有游标"正是它要挡的情形)。
  //   - 认跳变而非当前值:上面那次"可见"会一直留着,直到哨兵真的移出视口浏览器才再回调一次;
  //     只看当前值的话,内容一到位就会被那次**陈旧的可见**打开。
  //   内容不足一屏的情形不靠这条(哨兵始终可见、根本没有跳变),由信号 ②(贴底几何)兜住。
  const wasSentinelInView = useRef(false);
  useEffect(() => {
    const justBecameVisible = sentinelInView && !wasSentinelInView.current;
    wasSentinelInView.current = sentinelInView;
    if (justBecameVisible && items.length > 0) setReachedTail(true);
  }, [sentinelInView, items.length]);

  // ② 的实现:读滚动容器的三条几何值,贴到底(容差见 TAIL_SLACK_PX)就置真。
  // 挂载时先判一次 —— 内容不足一屏时(单条一页)根本没有滚动,这一刻就已经在底部;
  // 每追加一页后重判一次(items.length 在依赖里);再挂 scroll(滚动中)与 resize
  // (旋转屏幕 / 拉窗口会改 clientHeight,可能"变到底了"却不产生任何 scroll 事件)。
  // passive: 这个监听什么都不拦,滚动线程不该为它等主线程。
  useEffect(() => {
    const el = scrollerRef.current;
    // 一条都没有的时候**不判**:此刻滚动区里只有那个 1px 哨兵,任何容差下都"已经在底部",
    // 挂载那一刻就会把这道门打开 —— 那这道门就等于不存在(首屏还没回来时尤其如此)。
    if (!el || items.length === 0) return;

    const nearBottom = () => {
      if (el.scrollTop + el.clientHeight >= el.scrollHeight - TAIL_SLACK_PX) setReachedTail(true);
    };
    nearBottom();
    el.addEventListener('scroll', nearBottom, { passive: true });
    window.addEventListener('resize', nearBottom);
    return () => {
      el.removeEventListener('scroll', nearBottom);
      window.removeEventListener('resize', nearBottom);
    };
  }, [items.length]);

  // 状态归一。优先级:加载中 > 失败 > 到底 > (没有观察者时的)手动出口 > 空。
  // 重试 / 继续加载在飞的时候应该显示"加载中…",而不是继续摆着上一条提示(它此刻已经过期了)。
  const empty = items.length === 0;
  // 还有下一页,可这个环境没人能自动去要它(没有 IntersectionObserver ⇒ 哨兵永不触发)。
  // 这一态必须给出口:否则首屏恰好是"空页 + 有游标"时,屏幕上就是一句永远的"加载中…"。
  const manualOnly = !canObserve && !loading && !failed && !done;
  // 到底,但用户还没到过尾部 ⇒ 先不摆(见上面那两段注释)。注意 `empty` 不受这道门影响:
  // 一条都没有的时候屏幕上必须有话说,空态文案不摆出来就真的是一片空了。
  const atEnd = done && (empty || reachedTail);
  const state: 'loading' | 'error' | 'end' | 'manual' | null = loading
    ? 'loading'
    : failed
      ? 'error'
      : done
        ? atEnd
          ? 'end'
          : null
        : manualOnly
          ? 'manual'
          : // 一条都没有、也还没到底:屏幕上此刻什么都没有,而容器确实还在往下一段走
            // (等哨兵把下一页拉起来)。这一帧显示"加载中…"比留一片黑诚实 —— 首屏那一帧会落到这里。
            empty
            ? 'loading'
            : null;

  const stateContent =
    state === 'loading' ? (
      LOADING_TEXT
    ) : state === 'error' ? (
      // 整条文案就是按钮:点哪儿都能重试,不用去瞄那几个字。
      <button type="button" data-testid="feed-list-retry" onClick={() => void loadPage(cursor)} style={statusButtonStyle}>
        {ERROR_TEXT}
      </button>
    ) : state === 'manual' ? (
      // 没有 IntersectionObserver 的环境:哨兵永远不会触发,所以把"触底加载"这件事的出口
      // 显式做出来 —— 一个真按钮(可聚焦、有名字、键盘可达),而不是一句永远的"加载中…"。
      <button type="button" data-testid="feed-list-more" onClick={() => void loadPage(cursor)} style={statusButtonStyle}>
        {MORE_TEXT}
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
          明确留给了外层列表容器(它自己只管撑满一屏 + 撑多高)。
          className 只为一件事:把滚动条本身藏掉(规则在 feed.css,伪元素 inline 表达不了)。 */}
      <div ref={scrollerRef} data-testid="feed-list" className="feed-scroller" style={scrollerStyle}>
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
          // 失败态与"没有观察者"态里都是一个真按钮,由按钮自己说话(它有名字、能聚焦),
          // 别让容器再报一遍 —— 也不该把一个可聚焦控件塞进 live region 里。
          role={state === 'error' || state === 'manual' ? undefined : 'status'}
          style={empty ? fullScreenStatusStyle : tailStatusStyle}
        >
          {stateContent}
        </div>
      )}
    </div>
  );
}

// 沉浸底 + 自己撑满一屏:不依赖父容器高度(父容器若是 auto 高,height:100% 会算成 0,
// 整块塌掉 —— 与 VideoPanel 同一条理由、同一套口径)。
// 高度本身取 theme/tokens.ts 的 FULL_VIEWPORT_HEIGHT(那一对值的理由写在那儿,只此一份)。
const rootStyle: CSSProperties = {
  position: 'relative',
  // 与 MainLayout 的外壳、VideoPanel 的面板**同一个定义**:三者必须相等 —— 面板是一屏一屏
  // 吸附的,任何一处差一条,视频就会跟它所在的那一屏错开、底部露出一条底色。
  ...FULL_VIEWPORT_HEIGHT,
  width: '100%',
  // 浮层要钉在视口上而不是被内容顶开,所以溢出在这里截断,滚动交给里面那层
  overflow: 'hidden',
  background: IMMERSIVE_BG,
};

const scrollerStyle: CSSProperties = {
  height: '100%',
  overflowY: 'auto',
  // 滚动条本身由 .feed-scroller(feed.css)藏掉:classic 滚动条会在滚动区里侧占掉约 15px,
  // 于是视频右缘之外露出一条底色 —— "满屏出血"只是近似。滚动行为全留在这里(滚轮 / 触摸 /
  // 键盘 / 吸附一样不少),只是不再有一条占版面的滑块。
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
  top: 0,
  right: 0,
  bottom: 0,
  // 左边让开那条浮层导航(MainLayout 里 fixed 的 SIDEBAR_WIDTH):这个浮层是按**自己的盒子**
  // 居中的,盒子从视口最左起画的话,窄屏(375px)下居中的那行字正好压在导航底下 ——
  // 而那行字里就是"点此重试",只露一小截可点的等于死路。
  // 为什么不是"整页缩窄":视频必须铺满可视区(spec §7/§12,用户明确选的几何),缩窄已被否掉一次。
  left: SIDEBAR_WIDTH,
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'center',
  // 24 而不是 36:盒子让开导航之后窄屏只剩 175px 宽,36 的两侧内边距会把文案挤成两行
  // (重试按钮的文案 9 个字 ≈117px)—— 24 是刻度上的下一档,放得下整条。
  padding: spacing.lg,
  textAlign: 'center',
  color: TEXT_SECONDARY,
  fontSize: 14,
  // 整条文案都可能比盒子宽(长空态文案),长单词 / 长串不该顶破浮层。
  overflowWrap: 'anywhere',
  // 浮层本身不吃点击(它盖着整屏),但里面的重试 / 继续加载按钮自己把 pointer-events 打开。
  pointerEvents: 'none',
};

// 已经有视频在屏上:提示退成底部居中的一小条 —— 不该盖住画面,也不该抢视线。
const tailStatusStyle: CSSProperties = {
  position: 'absolute',
  // 贴到 4px:VideoPanel 左下角的信息块是从底边 36px 起画的(它自己的 padding),
  // 压到 4px 就不会盖在那几行字上;右侧操作栏在右半边,居中的小条也碰不到它。
  bottom: spacing.xxs,
  // 同样让开左侧浮层导航:原来是 left:50% + translateX(-50%),那是按**整宽**居中 ——
  // 窄屏下这一条会从左半边起画,压在导航底下。改成"在导航右边那一条里居中"(left + right + auto 外边距)。
  left: SIDEBAR_WIDTH,
  right: 0,
  marginLeft: 'auto',
  marginRight: 'auto',
  // width 必须是"非 auto",靠 auto 外边距居中的那条路才成立:left/right 都给了死值、width 又是 auto 时,
  // CSS 会把这两个 auto 外边距算成 0、让盒子**填满**那一段 —— 那就不是"底部一小条",而是横贯画面的黑带。
  width: 'fit-content',
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
  overflowWrap: 'anywhere',
  pointerEvents: 'none',
};

// 状态浮层里那两个真按钮("加载失败,点此重试" / "加载更多")共用一份样式:
// 它们出现在同一个位置、承担同一件事(把路走下去),长得不一样只会让人以为是两种东西。
const statusButtonStyle: CSSProperties = {
  display: 'inline-flex',
  alignItems: 'center',
  // 触摸目标给到 44(与 ActionRail 48 的 SLOT_MIN_SIZE 同一个理由):文案只有 13px,
  // 不留够可点面积,手指就点不中 —— 而这是失败 / 没有观察者时唯一的出路。
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
