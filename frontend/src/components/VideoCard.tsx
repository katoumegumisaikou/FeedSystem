import { Card, Tag, Typography } from 'antd';
import type { VideoResp } from '../types/video';
import { VideoStatus } from '../types/video';

// 状态码 → 中文标签。只用给「非已发布」的状态打标:已发布是常态,不打扰。
const STATUS_LABEL: Record<number, string> = {
  [VideoStatus.Processing]: '转码中',
  [VideoStatus.Failed]: '转码失败',
  [VideoStatus.Removed]: '已下架',
  [VideoStatus.Draft]: '草稿',
};

// props 类型单独 export:以后别的页面要用也能 import,不用重写
export interface VideoCardProps {
  video: VideoResp;
}

// 后端给的时间戳是 ISO 字符串,转成本地可读时间。
// 放在组件外是因为它是纯函数、和组件状态无关 —— 每次渲染重新定义没必要。
function formatTime(iso: string): string {
  return new Date(iso).toLocaleString('zh-CN', { hour12: false });
}

// 一张视频卡片。抽成独立组件是为了复用:
// 用户主页现在用它,以后的「最新」「关注流」列表可以直接 import,不必抄一遍。
// 组件只负责「把一条数据渲染成卡片」,不关心数据从哪来 —— 这样它才能被到处复用。
export default function VideoCard({ video }: VideoCardProps) {
  return (
    <Card
      hoverable
      cover={
        // cover_url 是站内相对路径(形如 /covers/{videoID}/{文件名}),
        // dev 下靠 vite proxy 转发到后端,所以直接写相对路径即可,不用拼域名。
        <img
          alt={video.title}
          src={video.cover_url}
          style={{ width: '100%', height: 180, objectFit: 'cover', background: '#f0f2f5' }}
        />
      }
    >
      <Card.Meta
        title={
          <span>
            {video.title}
            {/* 已发布(1)时不打标;草稿 / 转码中等状态需要提示,否则作者会困惑 */}
            {STATUS_LABEL[video.status] && (
              <Tag color="orange" style={{ marginLeft: 8 }}>
                {STATUS_LABEL[video.status]}
              </Tag>
            )}
          </span>
        }
        description={
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            ▶ {video.play_count} · ♥ {video.likes_count} · {formatTime(video.created_at)}
          </Typography.Text>
        }
      />
    </Card>
  );
}
