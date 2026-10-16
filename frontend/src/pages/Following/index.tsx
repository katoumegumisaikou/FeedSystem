import { type CSSProperties } from 'react';
import { listFollowingVideos } from '../../api/feed';
import FeedList from '../../components/feed/FeedList';
import { SIDEBAR_WIDTH } from '../../theme/tokens';
import type { FeedCursor, FeedItem } from '../../types/feed';

// spec §13 的空态文案,逐字搬过来 —— 关注流的"没有内容"和别的流不是一回事:
// 空的原因多半是"还没关注人",给的出路也因此是去主页看看,而不是去上传。
const EMPTY_TEXT = '你还没有关注的人 · 去主页看看';

// 取数适配器放在**模块级**:它必须是稳定引用(理由见 pages/Home)。
//
// 关注流的游标形状与最新流同构(都是 FeedCursor),但那是巧合:这里的 created_at 承载的是
// **发布时刻**(收件箱 ZSET 的 score),不是上传时刻。所以不能和 Home 共用一个函数 ——
// 一旦后端给关注流换了排序键,改动的边界应当是这一页。
//
// ⚠️ "是否到底"这里**不判**:只看容器(FeedList)的结论 —— 游标缺失才算到底,
// 绝不能看 items 的长度(空页也可能是"游标之后还有内容")。页面只负责把 next_cursor
// 改名成容器要的 nextCursor,原样递过去。
async function loadFollowingPage(
  cursor?: FeedCursor,
): Promise<{ items: FeedItem[]; nextCursor?: FeedCursor }> {
  const { items, next_cursor } = await listFollowingVideos(cursor);
  return { items, nextCursor: next_cursor };
}

// 页面自己让出左侧导航那一条 —— 理由见 pages/Home(导航是 fixed 浮层,状态浮层按页面的盒子居中)。
const pageStyle: CSSProperties = { marginLeft: SIDEBAR_WIDTH };

/** 关注流:只看我关注的人(GET /videos/following,**需登录** —— 无 token 后端直接 401)。 */
export default function FollowingPage() {
  return (
    <div data-testid="feed-page" style={pageStyle}>
      <FeedList<FeedCursor> load={loadFollowingPage} emptyText={EMPTY_TEXT} />
    </div>
  );
}
