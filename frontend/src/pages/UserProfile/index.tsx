import { useCallback, useEffect, useState } from 'react';
import { useParams } from 'react-router-dom';
import { Avatar, Button, Card, Col, Empty, Row, Spin, Typography } from 'antd';
import { getUserProfile } from '../../api/user';
import { listUserVideos } from '../../api/video';
import VideoCard from '../../components/VideoCard';
import type { PublicUserResp } from '../../types/user';
import type { UserVideoCursor, VideoResp } from '../../types/video';

function formatJoinDate(iso: string): string {
  return new Date(iso).toLocaleDateString('zh-CN');
}

export default function UserProfilePage() {
  // ① 从 URL 取参数:路由写成 /users/:id,这里就能拿到这个 id。
  //    注意 useParams 拿到的永远是**字符串**,而后端期望数字,
  //    所以下面 Number(id) 转换。传 "abc" 会被后端判「用户 ID 无效」。
  const { id } = useParams<{ id: string }>();
  const userId = Number(id);
  const validId = Number.isInteger(userId) && userId > 0;

  // ② 用 state 存「服务器数据」。React 组件的渲染是纯函数,
  //    要显示的数据变了,就得通过 setState 触发重新渲染。
  const [user, setUser] = useState<PublicUserResp | null>(null);
  const [userLoading, setUserLoading] = useState(true);

  const [videos, setVideos] = useState<VideoResp[]>([]);
  const [videosLoading, setVideosLoading] = useState(true);
  // next_cursor 为 undefined 表示「没有下一页了」——用它决定「加载更多」按钮显不显示
  const [nextCursor, setNextCursor] = useState<UserVideoCursor | undefined>(undefined);

  // ③ useEffect:组件渲染后要做的「副作用」(发请求就是典型)。
  //    依赖数组 [userId] 表示:只在 userId 变化时重跑 —— 否则每次渲染都发请求会死循环。
  //    effect 回调不能直接写成 async(async 会返回 Promise,React 不认),所以套一个
  //    立即执行的 async 函数: (async () => { ... })()
  useEffect(() => {
    if (!validId) return;

    (async () => {
      setUserLoading(true);
      try {
        const data = await getUserProfile(userId);
        setUser(data);
      } catch {
        // 错误提示已由 axios 拦截器统一弹出,这里吞掉,避免未处理的 Promise rejection
      } finally {
        setUserLoading(false);
      }
    })();
  }, [userId, validId]);

  useEffect(() => {
    if (!validId) return;

    (async () => {
      setVideosLoading(true);
      try {
        const resp = await listUserVideos(userId);
        setVideos(resp.items);
        setNextCursor(resp.next_cursor);
      } catch {
        // 同上,拦截器已弹错
      } finally {
        setVideosLoading(false);
      }
    })();
  }, [userId, validId]);

  // ④ 「加载更多」:拿上一页的游标接着拉,结果**追加**到列表尾部(不是替换)。
  //    用 useCallback 是为了让函数引用稳定,依赖 nextCursor —— 每拉到新游标就更新自己。
  const loadMore = useCallback(async () => {
    if (!nextCursor) return;
    setVideosLoading(true);
    try {
      const resp = await listUserVideos(userId, nextCursor);
      setVideos((prev) => [...prev, ...resp.items]);
      setNextCursor(resp.next_cursor);
    } catch {
      // 错误提示已由 axios 拦截器弹出,这里吞掉
    } finally {
      setVideosLoading(false);
    }
  }, [userId, nextCursor]);

  // ⑤ 渲染的「分岔」放在所有 hooks 之后(hooks 不能有条件地调用,否则 React 会报错)
  if (!validId) {
    return <Empty description="用户 ID 无效" style={{ padding: 48 }} />;
  }
  if (userLoading && !user) {
    return (
      <div style={{ textAlign: 'center', padding: 48 }}>
        <Spin />
      </div>
    );
  }
  if (!user) {
    return <Empty description="用户不存在" style={{ padding: 48 }} />;
  }

  // 作品列表:没有作品且不在加载 → 空态;否则铺成响应式网格。
  // key 用视频 id(稳定),不能用数组下标 —— 否则插入/删除时 React 会错认元素
  const videoList =
    videos.length === 0 && !videosLoading ? (
      <Empty description="还没有发布作品" />
    ) : (
      <Row gutter={[16, 16]}>
        {videos.map((v) => (
          <Col key={v.id} xs={24} sm={12} md={8}>
            <VideoCard video={v} />
          </Col>
        ))}
      </Row>
    );

  return (
    <div style={{ maxWidth: 960, margin: '0 auto', padding: 24 }}>
      {/* 用户信息头部 */}
      <Card style={{ marginBottom: 24 }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 16 }}>
          {/* 有 avatar_url 就显示图片,没有就用用户名首字兜底 */}
          <Avatar size={64} src={user.avatar_url}>
            {user.user_name?.[0]}
          </Avatar>
          <div>
            <Typography.Title level={4} style={{ margin: 0 }}>
              {user.user_name}
            </Typography.Title>
            <Typography.Text type="secondary">
              加入于 {formatJoinDate(user.created_at)}
            </Typography.Text>
          </div>
        </div>
      </Card>

      <Typography.Title level={5}>TA 的作品</Typography.Title>

      {videoList}

      <div style={{ textAlign: 'center', marginTop: 24 }}>
        {nextCursor ? (
          <Button onClick={loadMore} loading={videosLoading}>
            加载更多
          </Button>
        ) : (
          videos.length > 0 && <Typography.Text type="secondary">没有更多了</Typography.Text>
        )}
      </div>
    </div>
  );
}
