import axios, { type InternalAxiosRequestConfig } from 'axios';
import http, { type RequestConfig } from './axios';
import { followUser, listFollowees, unfollowUser } from './follow';

// 与 api/feed.test.ts 同一套做法:换掉 axios 实例的 adapter —— 那是 axios 真正"发请求"的地方,
// 进到这里的 config 已经过 baseURL 拼接,拿到的就是线上那份请求。断言的是
// 「路径 + 方法 + 参数」这三件事,而不是把被测代码里的路径常量再念一遍
// (那样测试和实现一起改就永远不红)。顺带覆盖了"响应拦截器解包 {code,msg,data}"这条链路。
async function capture<T>(
  call: () => Promise<T>,
  payload: unknown,
): Promise<{ sent: InternalAxiosRequestConfig; result: T }> {
  let sent: InternalAxiosRequestConfig | undefined;
  http.defaults.adapter = (config) => {
    sent = config;
    return Promise.resolve({
      data: { code: 0, msg: 'ok', data: payload },
      status: 200,
      statusText: 'OK',
      headers: {},
      config,
    });
  };

  const result = await call();
  if (!sent) throw new Error('没有发出请求');
  return { sent, result };
}

describe('follow 关注关系封装', () => {
  it('listFollowees:GET /users/me/followees(我关注的所有人,一次给全),且是静默的后台读取', async () => {
    const payload = { followee_ids: [3, 7], limit: 5000 };
    const { sent, result } = await capture(listFollowees, payload);

    expect(sent.method).toBe('get');
    expect(axios.getUri(sent)).toBe('/api/v1/users/me/followees');
    // 这条是挂载时自动触发的:游客进首页必然拿 401,不能因此弹一次"请先登录"
    // (他什么都没点)。silent 走的是 axios 实例上的自定义开关,由拦截器读取。
    expect((sent as RequestConfig).silent).toBe(true);
    // 调用方拿到的是解包后的业务数据,不是 AxiosResponse
    expect(result).toEqual(payload);
  });

  it('followUser:POST /users/:id/follow,失败要说话(不是后台探测)', async () => {
    const payload = { following: true, follower_count: 12 };
    const { sent, result } = await capture(() => followUser(7), payload);

    expect(sent.method).toBe('post');
    // 断言拼出来的真实 URL(baseURL + 路径),而不是断一个 '/' + id 的字符串常量
    expect(axios.getUri(sent)).toBe('/api/v1/users/7/follow');
    // 这几条是用户主动点的动作,失败必须弹提示(游客点关注 → "请先登录")
    expect((sent as RequestConfig).silent).toBeUndefined();
    expect(result).toEqual(payload);
  });

  it('unfollowUser:DELETE /users/:id/follow', async () => {
    const payload = { following: false, follower_count: 11 };
    const { sent, result } = await capture(() => unfollowUser(7), payload);

    expect(sent.method).toBe('delete');
    expect(axios.getUri(sent)).toBe('/api/v1/users/7/follow');
    expect(result).toEqual(payload);
  });
});
