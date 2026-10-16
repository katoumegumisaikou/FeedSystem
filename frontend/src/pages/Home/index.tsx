import { listLatestVideos } from '../../api/feed';
import FeedList from '../../components/feed/FeedList';
import type { FeedCursor, FeedItem } from '../../types/feed';

// spec §13 的空态文案,逐字搬过来。
const EMPTY_TEXT = '还没有视频 · 去上传你的第一条';

// 取数适配器放在**模块级**:它必须是稳定引用。
// FeedList 把 load 放进 useCallback 的依赖里,内联箭头每次渲染都是新函数 ⇒ 引用一变那个
// effect 就重跑,可能多发一次尾页请求。模块级函数天然只创建一次。
//
// 适配器只做一件翻译:把 API 的 next_cursor 改名成容器要的 nextCursor —— 容器不 import
// 任何 feed api(它把游标当不透明值),所以"响应长什么样"和"容器要什么"的对接点就在这一行。
async function loadLatestPage(
  cursor?: FeedCursor,
): Promise<{ items: FeedItem[]; nextCursor?: FeedCursor }> {
  const { items, next_cursor } = await listLatestVideos(cursor);
  return { items, nextCursor: next_cursor };
}

// 页面**不**给导航留内缩:导航是 fixed 浮层(不占布局宽度),页面一内缩就等于把**视频本身**挤窄,
// "铺满可视区"(spec §7/§12)当场不成立。避让只落在"浮在视频上的东西"上 —— 见 VideoPanel
// 给左下角文字留的左内边距。
//
// (待办,Task 11:FeedList 那个"按自己盒子居中"的四态浮层在窄屏会落到导航底下,
//  该由**浮层自己**让开,而不是把整页缩窄。)

/** 主页:最新视频流(GET /videos/latest)。游客可看 —— 后端这条是软鉴权。 */
export default function HomePage() {
  return (
    <div data-testid="feed-page">
      <FeedList<FeedCursor> load={loadLatestPage} emptyText={EMPTY_TEXT} />
    </div>
  );
}
