import { render, screen } from '@testing-library/react';
import { ConfigProvider, theme } from 'antd';
import { darkTokens, spacing } from './tokens';

// 这些值来自 spec §13 的「深色 token 起始值」表 —— 它们决定了新 feed 页面长什么样,
// 又只有这一处定义,所以用测试钉死:值被改动时必须有人为它负责。
describe('深色设计 token', () => {
  it('沉浸底与主文字用 spec §13 的值', () => {
    expect(darkTokens.token?.colorBgBase).toBe('#0b0b0f');
    expect(darkTokens.token?.colorTextBase).toBe('#f5f5f7');
  });

  it('表面与文字层级都显式落在深色上,不会掉回浅色兜底值', () => {
    expect(darkTokens.token?.colorBgContainer).toBe('#16161c');
    expect(darkTokens.token?.colorBgElevated).toBe('#1c1c24');
    expect(darkTokens.token?.colorBorder).toBe('#2c2c36');
    expect(darkTokens.token?.colorText).toBe('#f5f5f7');
    expect(darkTokens.token?.colorTextSecondary).toBe('#a1a1aa');
    expect(darkTokens.token?.colorTextTertiary).toBe('#71717a');
  });

  it('圆角取 spec 的 8 / 12', () => {
    expect(darkTokens.token?.borderRadius).toBe(8);
    expect(darkTokens.token?.borderRadiusLG).toBe(12);
  });

  it('不新造强调色 —— 让 antd 默认的 colorPrimary 透过来,和浅色页同一套品牌色', () => {
    expect(darkTokens.token?.colorPrimary).toBeUndefined();
  });

  it('间距刻度按 spec §13 的 6 档导出(antd 没有 spacing token,只能自己给)', () => {
    expect(spacing).toEqual({ xxs: 4, xs: 8, sm: 12, md: 16, lg: 24, xl: 36 });
  });
});

// 上面几条只证明常量写对了;下面这条证明它**接到 antd 上真的生效**,而且只影响子树 ——
// 这正是 spec §7「深色只属于 feed 页,登录 / 注册 / 个人主页保持浅色」的落点,
// 也是 Task 9 的 MainLayout 嵌套 <ConfigProvider theme={darkTokens}> 的依据。
function TokenProbe({ testId }: { testId: string }) {
  const { token } = theme.useToken();

  return (
    <span data-testid={testId}>
      {[token.colorBgBase, token.colorBgContainer, token.colorTextBase, token.colorPrimary].join('|')}
    </span>
  );
}

describe('darkTokens 在 ConfigProvider 里的行为', () => {
  it('嵌套使用时深色只作用于子树,父层浅色页保持原样', () => {
    render(
      <ConfigProvider>
        <TokenProbe testId="light" />
        <ConfigProvider theme={darkTokens}>
          <TokenProbe testId="dark" />
        </ConfigProvider>
      </ConfigProvider>,
    );

    // 子树:三个颜色都是深色值,colorPrimary 仍是 antd 默认(与浅色页同一个品牌色)
    expect(screen.getByTestId('dark')).toHaveTextContent('#0b0b0f|#16161c|#f5f5f7|#1677ff');
    // 父层:一个值都没被改 —— 浅色页不会被刷黑
    expect(screen.getByTestId('light')).not.toHaveTextContent('#0b0b0f');
    expect(screen.getByTestId('light')).not.toHaveTextContent('#f5f5f7');
    // 两边的强调色一致,证明没有新造品牌色
    expect(screen.getByTestId('light')).toHaveTextContent('#1677ff');
  });
});
