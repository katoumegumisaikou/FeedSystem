import { useState } from 'react';
import { App, Form, Input, Button, Space } from 'antd';
import { Link, useNavigate as useRouterNavigate } from 'react-router-dom';
import AuthLayout from '../../layouts/AuthLayout';
import SmsCodeButton from '../../components/SmsCodeButton';
import { register } from '../../api/auth';
import { invalidateFollowees } from '../../hooks/useFollowees';
import type { RegisterReq } from '../../types/auth';

// 字段校验与后端对齐:user_name 对齐 username.Validate,password 对齐 password.Strong
export default function RegisterPage() {
  const [loading, setLoading] = useState(false);
  const [form] = Form.useForm<RegisterReq>();
  const router = useRouterNavigate();
  const { message } = App.useApp();

  const onFinish = async (values: RegisterReq) => {
    setLoading(true);
    try {
      await register(values);
      // 身份变了:同 pages/Login —— 游客阶段那份(空的)"我关注了谁"必须作废,否则登录后不会再拉。
      invalidateFollowees();
      message.success('注册成功,已自动登录');
      router('/');
    } catch {
      // 拦截器已处理
    } finally {
      setLoading(false);
    }
  };

  return (
    <AuthLayout title="注册">
      <Form<RegisterReq>
        form={form}
        layout="vertical"
        onFinish={onFinish}
        autoComplete="off"
      >
        <Form.Item
          name="user_name"
          label="用户名"
          rules={[
            { required: true, message: '请输入用户名' },
            { pattern: /^[A-Za-z][A-Za-z0-9_]*[A-Za-z0-9]$/, message: '字母开头,字母数字下划线,需含字母' },
            { min: 3, max: 32, message: '长度 3-32' },
          ]}
        >
          <Input placeholder="字母开头,3-32 位" />
        </Form.Item>

        <Form.Item
          name="phone"
          label="手机号"
          rules={[
            { required: true, message: '请输入手机号' },
            { pattern: /^1[3-9]\d{9}$/, message: '手机号格式不正确' },
          ]}
        >
          <Input placeholder="11 位手机号" maxLength={11} />
        </Form.Item>

        <Form.Item shouldUpdate noStyle>
          {() => (
            <Form.Item
              name="sms_code"
              label="验证码"
              rules={[
                { required: true, message: '请输入验证码' },
                { len: 6, message: '验证码为 6 位数字' },
              ]}
            >
              <Space.Compact style={{ width: '100%' }}>
                <Input placeholder="6 位短信验证码" maxLength={6} />
                <SmsCodeButton
                  phone={form.getFieldValue('phone')}
                  onError={(msg) => message.error(msg)}
                />
              </Space.Compact>
            </Form.Item>
          )}
        </Form.Item>

        <Form.Item
          name="password"
          label="密码"
          rules={[
            { required: true, message: '请输入密码' },
            { min: 8, max: 16, message: '密码 8-16 位' },
            // 对齐后端 password.Strong:三类至少两类
            {
              validator: async (_, value: string) => {
                if (!value) return Promise.resolve();
                // 只允许 ASCII 可打印字符(排除空格),对齐后端 password.Strong
                if (!/^[\x21-\x7E]+$/.test(value)) {
                  return Promise.reject(new Error('密码只能使用字母、数字和常见符号'));
                }
                const types = [/[a-zA-Z]/.test(value), /\d/.test(value), /[^a-zA-Z0-9]/.test(value)];
                const cnt = types.filter(Boolean).length;
                return cnt >= 2 ? Promise.resolve() : Promise.reject(new Error('需含字母、数字、特殊字符中的至少两类'));
              },
            },
          ]}
          hasFeedback
        >
          <Input.Password placeholder="字母+数字+特殊字符,至少 2 类" />
        </Form.Item>

        <Form.Item
          name="email"
          label="邮箱"
          rules={[{ type: 'email', message: '邮箱格式不正确' }]}
        >
          <Input placeholder="可选" />
        </Form.Item>

        <Form.Item>
          <Button type="primary" htmlType="submit" loading={loading} block>
            注册并登录
          </Button>
        </Form.Item>
      </Form>

      <div style={{ textAlign: 'center' }}>
        已有账号?<Link to="/login">直接登录</Link>
      </div>
    </AuthLayout>
  );
}