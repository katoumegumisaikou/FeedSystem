import { type CSSProperties } from 'react';
import { listLatestVideos } from '../../api/feed';
import FeedList from '../../components/feed/FeedList';
import { SIDEBAR_WIDTH } from '../../theme/tokens';
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

// 页面自己让出左侧导航那一条:外壳的导航是 fixed 浮层,不占布局宽度,这一条只能由页面给。
//
// 为什么必须让:FeedList 的四态浮层(加载中 / 加载失败+重试 / 空态)是按**自己的盒子**居中的
// (inset: 0 相对它的根)。盒子从视口最左边开始的话,手机宽度下居中的那行字正好压在导航底下 ——
// 那行字里还有"点此重试",点不到就是个死路。
//
// 为什么让 SIDEBAR_WIDTH 就够、也不亏:导航是不透明的固定浮层、盖住的就是这一条,
// 让出去的这块本来也看不见(内容并没有真的少一块)。
const pageStyle: CSSProperties = { marginLeft: SIDEBAR_WIDTH };

/** 主页:最新视频流(GET /videos/latest)。游客可看 —— 后端这条是软鉴权。 */
export default function HomePage() {
  return (
    <div data-testid="feed-page" style={pageStyle}>
      <FeedList<FeedCursor> load={loadLatestPage} emptyText={EMPTY_TEXT} />
    </div>
  );
}
