import { render, screen } from '@testing-library/react';

// 基建冒烟测试:验证 jsdom 环境、全局 describe/it/expect 与 jest-dom 匹配器都就位。
// 它不测任何业务逻辑,只证明 `npm test` 真的能跑起来 —— 后面的 TDD 任务都靠这套基建。
describe('测试基建', () => {
  it('能渲染组件并断言 DOM 内容', () => {
    render(<div>ok</div>);

    expect(screen.getByText('ok')).toBeInTheDocument();
  });
});
