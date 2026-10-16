import { useState } from 'react';
import { App, Form, Input, Button, Divider } from 'antd';
import { Link, useLocation, useNavigate as useRouterNavigate } from 'react-router-dom';
import AuthLayout from '../../layouts/AuthLayout';
import { login } from '../../api/auth';
import { invalidateFollowees } from '../../hooks/useFollowees';
import type { LoginReq } from '../../types/auth';

// 登录成功后的落地页:默认是主页 `/`(spec §11 —— 不再跳"个人主页"),
// 但若这一趟是被"需登录"的守卫送进来的,它会把自己的原址放进 router state,这时回原址。
//
// 为什么用 router state 而不是 `?next=` 查询参数:地址栏不必被一个跳转用的参数污染,
// 而且这份状态是我们自己写进去的,不是用户可以拼出来的输入。
//
// 只认**站内相对路径**:绝对地址(http://…)与协议相对地址(//host)一律丢掉,退回默认落地页。
// 这份 state 是本应用自己写的,但校验不能省:一个"照参数跳转"的入口,只要将来有一处
// 能让外部值进来,它当场就变成一个开放重定向。
function landingPath(state: unknown): string {
  const from = (state as { from?: unknown } | null | undefined)?.from;
  return typeof from === 'string' && from.startsWith('/') && !from.startsWith('//') ? from : '/';
}

// 登录成功后后端通过 SetTokenCookies 写入 access_token / refresh_token
export default function LoginPage() {
  const [loading, setLoading] = useState(false);
  const [form] = Form.useForm<LoginReq>();
  const router = useRouterNavigate();
  // 守卫送人过来时带上的原址;直接打开登录页时它是 undefined。
  const location = useLocation();
  const { message } = App.useApp();

  const onFinish = async (values: LoginReq) => {
    setLoading(true);
    try {
      await login(values);
      // 身份变了:上一份「我关注了谁」不再属于当前用户。游客阶段那次静默 401 已经把缓存
      // 标成"拉过了",不在这里作废的话,登录后每个作者都会永远显示"关注"。
      invalidateFollowees();
      message.success('登录成功');
      // 落地页从"个人主页"改成主页 /(spec §11)之后,这里不再需要 /users/me 那一次探测 ——
      // 它存在的唯一理由就是拼 /users/{me.id},现在没有用处了(少一次请求)。
      //
      // replace:登录页不该在历史里留一格 —— 登录成功还按返回键回到登录表单是说不通的。
      router(landingPath(location.state), { replace: true });
    } catch {
      // 拦截器已弹错(api/axios.ts 会把后端的 msg 弹成一条 message)—— 这里不能再补一条:
      // 同一个失败弹两条提示,用户看到的是两条互不相同的说法,而且两条叠在一起。
    } finally {
      setLoading(false);
    }
  };

  return (
    <AuthLayout title="登录">
      <Form<LoginReq>
        form={form}
          layout="vertical"
          onFinish={onFinish}
          autoComplete="off"
        >
        <Form.Item
          name="phone"
          label="手机号"
          rules={[
            { required: true, message: '请输入手机号' },
            { pattern: /^1[3-9]\d{9}$/, message: '手机号格式不正确' },
          ]}
        >
          <Input placeholder="请输入 11 位手机号" maxLength={11} />
        </Form.Item>

        <Form.Item
          name="password"
          label="密码"
          rules={[
            { required: true, message: '请输入密码' },
            { min: 8, max: 16, message: '密码 8-16 位' },
          ]}
        >
          <Input.Password placeholder="请输入密码" />
        </Form.Item>

        <Form.Item>
          <Button type="primary" htmlType="submit" loading={loading} block>
            登录
          </Button>
        </Form.Item>
      </Form>

      <Divider style={{ margin: '12px 0' }} />

      <div style={{ textAlign: 'center' }}>
        <Link to="/forgot-password">忘记密码</Link>
      </div>

      <div style={{ textAlign: 'center', marginTop: 16 }}>
        还没有账号?<Link to="/register">立即注册</Link>
      </div>
    </AuthLayout>
  );
}