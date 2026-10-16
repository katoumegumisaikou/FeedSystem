import axios, { type InternalAxiosRequestConfig } from 'axios';
import http from './axios';
import { listFollowingVideos, listHotVideos, listLatestVideos } from './feed';

// 这一层没有业务逻辑,只有「路径 + 方法 + 参数」三件事,所以测试也必须断言这三件事,
// 而且要断言真正发出去的那份请求 —— 不能把被测代码里的常量再念一遍(那样测试和实现
// 一起改就永远不红)。
//
// 做法:替换 axios 实例的 adapter。adapter 是 axios 真正"发请求"的地方,进到这里的
// config 已经过 baseURL 拼接,拿到的就是线上的那份请求;比 mock 掉 request() 更接近真实,
// 也顺带覆盖了"响应拦截器解包 {code,msg,data}"这条链路。
async function captureRequest(call: () => Promise<unknown>): Promise<InternalAxiosRequestConfig> {
  let sent: InternalAxiosRequestConfig | undefined;
  http.defaults.adapter = (config) => {
    sent = config;
    // 后端统一格式:被拦截器解包后,调用方拿到的是 data 字段
    return Promise.resolve({
      data: { code: 0, msg: 'ok', data: { items: [], next_cursor: null } },
      status: 200,
      statusText: 'OK',
      headers: {},
      config,
    });
  };

  await call();
  if (!sent) throw new Error('没有发出请求');
  return sent;
}

describe('feed 取数封装', () => {
  it('listLatestVideos:GET /videos/latest,游标是 created_at', async () => {
    const sent = await captureRequest(() => listLatestVideos({ created_at: 1, video_id: 2 }, 5));

    expect(sent.method).toBe('get');
    expect(sent.params).toEqual({ limit: 5, cursor_created_at: 1, cursor_video_id: 2 });
    // 断言拼出来的真实 URL(baseURL + 路径 + query),而不是断 path 常量
    expect(axios.getUri(sent)).toBe('/api/v1/videos/latest?limit=5&cursor_created_at=1&cursor_video_id=2');
  });

  it('listHotVideos:GET /videos/like,游标是 likes_count(不是 created_at)', async () => {
    const sent = await captureRequest(() => listHotVideos({ likes_count: 9, video_id: 2 }, 5));

    expect(sent.method).toBe('get');
    expect(sent.params).toEqual({ limit: 5, cursor_likes_count: 9, cursor_video_id: 2 });
    expect(axios.getUri(sent)).toBe('/api/v1/videos/like?limit=5&cursor_likes_count=9&cursor_video_id=2');
  });

  it('listFollowingVideos:GET /videos/following,不传游标就不带游标参数', async () => {
    const sent = await captureRequest(() => listFollowingVideos());

    expect(sent.method).toBe('get');
    expect(sent.params).toEqual({ limit: 10, cursor_created_at: undefined, cursor_video_id: undefined });
    // 两个游标参数值为 undefined,axios 在拼 query 时会省略 —— 首页请求里不该出现 cursor
    expect(axios.getUri(sent)).toBe('/api/v1/videos/following?limit=10');
  });

  it('listFollowingVideos:带游标时两个参数成对出现', async () => {
    const sent = await captureRequest(() => listFollowingVideos({ created_at: 7, video_id: 8 }));

    expect(axios.getUri(sent)).toBe('/api/v1/videos/following?limit=10&cursor_created_at=7&cursor_video_id=8');
  });

  it('走 request<T>,调用方拿到的是解包后的业务数据', async () => {
    http.defaults.adapter = (config) =>
      Promise.resolve({
        data: { code: 0, msg: 'ok', data: { items: [{ id: 1 }], next_cursor: { created_at: 3, video_id: 1 } } },
        status: 200,
        statusText: 'OK',
        headers: {},
        config,
      });

    await expect(listFollowingVideos()).resolves.toEqual({
      items: [{ id: 1 }],
      next_cursor: { created_at: 3, video_id: 1 },
    });
  });
});
