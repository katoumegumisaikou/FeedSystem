import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { render, screen } from '@testing-library/react';
import { ConfigProvider, theme } from 'antd';
import { darkTokens, palette, spacing } from './tokens';

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

  // 这几项若不显式给,会被「浅色算法 + 深色种子」推成坏值(实测):
  // 描边/分隔线推成纯黑 → 看不见;布局底推成 #020204 → 比沉浸底还黑;
  // 气泡底推成近白 → 与它的白字撞成白字白底。所以每一项都得钉住。
  it('推导会出坏值的几项也显式钉住(描边/分隔线/布局底/气泡底)', () => {
    expect(darkTokens.token?.colorBorderSecondary).toBe('#2c2c36');
    expect(darkTokens.token?.colorSplit).toBe('rgba(245,245,247,0.10)');
    expect(darkTokens.token?.colorBgLayout).toBe('#0b0b0f');
    expect(darkTokens.token?.colorBgSpotlight).toBe('#2c2c36');
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

// palette 是**手写样式**那一半(浮在视频上的遮罩 / 文字 / 操作栏 / 关注按钮 / 底部提示条)的唯一
// 颜色来源:它们不在 antd 的 token 体系里(ConfigProvider 管不到 inline 样式),原来五个文件各抄一遍
// hex,改 token 时没人会想起它们 —— spec §7 的"同源"在手写样式上是断的。
describe('palette:手写样式的颜色只有这一份', () => {
  it('就是 spec §13「深色 token 起始值」里被用到的六个值', () => {
    expect(palette).toEqual({
      background: '#0b0b0f',
      surface: '#16161c',
      elevated: '#1c1c24',
      border: '#2c2c36',
      textPrimary: '#f5f5f7',
      textSecondary: '#a1a1aa',
    });
  });

  it('darkTokens 那几项取自 palette:antd 渲染的与手写的不会再分叉', () => {
    expect(darkTokens.token?.colorBgBase).toBe(palette.background);
    expect(darkTokens.token?.colorBgLayout).toBe(palette.background);
    expect(darkTokens.token?.colorBgContainer).toBe(palette.surface);
    expect(darkTokens.token?.colorBgElevated).toBe(palette.elevated);
    expect(darkTokens.token?.colorBorder).toBe(palette.border);
    expect(darkTokens.token?.colorText).toBe(palette.textPrimary);
    expect(darkTokens.token?.colorTextSecondary).toBe(palette.textSecondary);
  });
});

// feed.css 里那几条规则(焦点环 / 导航悬停 / 按下态 / 滚动条)只能用样式表写 —— 而样式表
// **import 不了 TS**,所以它那一半的"同源"只能靠这里的断言守住:样式表里出现的每个 hex
// 都必须是 palette 里的值。任何一边单独改掉一个 hex,这一组就会红(改哪个都得同时改另一边)。
//
// 路径用 process.cwd() 而不是 `new URL('./feed.css', import.meta.url)`:本工程跑在 jsdom 环境下,
// 那里的 import.meta.url 不是 file: 协议(readFileSync 会当场抛 "must be of scheme file");
// vitest 的工作目录就是工程根(`npm test` 在 frontend/ 下跑),所以 cwd + 相对路径是稳的。
// 路径写错时 readFileSync 会直接抛错 —— 这个文件当场红,不会静默跳过。
const FEED_CSS = readFileSync(resolve(process.cwd(), 'src/feed.css'), 'utf8');

describe('feed.css 与 palette 同源', () => {
  it('样式表里的每个 hex 都是 palette 里的值(一个都不许新造)', () => {
    const hexes = FEED_CSS.match(/#[0-9a-fA-F]{3,8}\b/g) ?? [];
    // 先证明"真的抓到了东西":一条都没抓到的话,下面的循环是空的,这条断言就成了摆设。
    expect(hexes.length).toBeGreaterThan(0);

    const known = new Set<string>(Object.values(palette));
    for (const hex of hexes) {
      expect(known.has(hex.toLowerCase())).toBe(true);
    }
  });

  it('样式表真正用到的那两个值被钉住(焦点环 / 导航悬停底)', () => {
    // 焦点环的亮色 = 主文字;导航悬停底 = 再亮一档的面。两条都是"改了 palette 就该跟着改"的地方,
    // 所以这里断的是**具体哪一条规则用了它**,不只是"文件里出现过某个 hex"。
    expect(FEED_CSS).toContain(`outline: 2px solid ${palette.textPrimary}`);
    expect(FEED_CSS).toContain(`background: ${palette.elevated}`);
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
