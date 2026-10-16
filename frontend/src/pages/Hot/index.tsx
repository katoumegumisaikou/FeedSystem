import { listHotVideos } from '../../api/feed';
import FeedList from '../../components/feed/FeedList';
import type { FeedItem, LikeCursor } from '../../types/feed';

// spec §13 的空态文案:热门流没有自己那一句,用通用那句(不自己造文案)。
const EMPTY_TEXT = '还没有视频 · 去上传你的第一条';

// 取数适配器放在**模块级**:它必须是稳定引用。
// FeedList 把 load 放进 useCallback 的依赖里,内联箭头每次渲染都是新函数 ⇒ 引用一变那个
// effect 就重跑,可能多发一次尾页请求。模块级函数天然只创建一次(理由同 pages/Home)。
//
// 适配器只做一件翻译:把 API 的 next_cursor 改名成容器要的 nextCursor。游标**原样收进来、
// 原样递回去** —— 它是 LikeCursor(likes_count, video_id),和最新流的 (created_at, video_id)
// 不是一回事,所以这个函数不能和 Home 共用(形状写错就会被后端判参数无效)。
async function loadHotPage(
  cursor?: LikeCursor,
): Promise<{ items: FeedItem[]; nextCursor?: LikeCursor }> {
  const { items, next_cursor } = await listHotVideos(cursor);
  return { items, nextCursor: next_cursor };
}

/** 热门:按点赞数排序的流(GET /videos/like)。游客可看。页面不内缩,理由见 pages/Home。 */
export default function HotPage() {
  return (
    <div data-testid="feed-page">
      <FeedList<LikeCursor> load={loadHotPage} emptyText={EMPTY_TEXT} />
    </div>
  );
}
