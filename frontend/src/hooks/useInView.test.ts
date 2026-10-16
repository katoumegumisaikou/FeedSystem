import { act, render, screen } from '@testing-library/react';
import { createElement } from 'react';
import { useInView } from './useInView';

// jsdom 没有 IntersectionObserver,而"可见性"这件事只能由浏览器推进。
// 所以这里装一个可遥控的替身:它把回调和被观察元素交到测试手上,测试就能像浏览器一样
// 主动发一次「滚进 / 滚出视口」。这样断言的是 hook 的行为(返回值跟着回调变、卸载时断开),
// 而不是"某个 mock 被 new 过"——后者就算 hook 忘了 setState 也照样绿。
class FakeIntersectionObserver {
  static instances: FakeIntersectionObserver[] = [];

  callback: IntersectionObserverCallback;
  options?: IntersectionObserverInit;
  target: Element | null = null;
  disconnected = false;

  constructor(callback: IntersectionObserverCallback, options?: IntersectionObserverInit) {
    this.callback = callback;
    this.options = options;
    FakeIntersectionObserver.instances.push(this);
  }

  observe(target: Element) {
    this.target = target;
  }

  disconnect() {
    this.disconnected = true;
  }

  // 模拟浏览器回调:元素按 threshold 进入 / 离开视口后,交出一条 entry
  emit(isIntersecting: boolean) {
    this.callback(
      [{ isIntersecting, target: this.target } as unknown as IntersectionObserverEntry],
      this as unknown as IntersectionObserver,
    );
  }

  static latest(): FakeIntersectionObserver {
    const observer = FakeIntersectionObserver.instances.at(-1);
    if (!observer) throw new Error('没有创建 IntersectionObserver');
    return observer;
  }
}

const realIntersectionObserver = globalThis.IntersectionObserver;

beforeEach(() => {
  FakeIntersectionObserver.instances = [];
  globalThis.IntersectionObserver = FakeIntersectionObserver as unknown as typeof IntersectionObserver;
});

afterEach(() => {
  globalThis.IntersectionObserver = realIntersectionObserver;
});

// 探针组件:把 hook 的返回值渲染进 DOM,这样断言的是"界面上看到的可见状态"。
// 用 createElement 而不是 JSX,是为了让测试文件保持 .ts(与 brief 里的文件名一致)。
function Probe({ threshold }: { threshold?: number }) {
  const [ref, inView] = useInView<HTMLDivElement>(threshold === undefined ? undefined : { threshold });
  return createElement('div', { ref }, inView ? '可见' : '不可见');
}

describe('useInView', () => {
  it('默认按 0.6 的阈值观察挂载出来的那个元素', () => {
    render(createElement(Probe));

    const observer = FakeIntersectionObserver.latest();
    expect(observer.options?.threshold).toBe(0.6);
    expect(observer.target).toBe(screen.getByText('不可见'));
  });

  it('阈值可覆盖', () => {
    render(createElement(Probe, { threshold: 0.2 }));

    expect(FakeIntersectionObserver.latest().options?.threshold).toBe(0.2);
  });

  it('返回值跟随回调:isIntersecting 为真变可见,再为假变回不可见', () => {
    render(createElement(Probe));
    const observer = FakeIntersectionObserver.latest();

    act(() => observer.emit(true));
    expect(screen.getByText('可见')).toBeInTheDocument();

    act(() => observer.emit(false));
    expect(screen.getByText('不可见')).toBeInTheDocument();
  });

  it('卸载时断开观察,避免元素销毁后回调还打到已卸载的组件上', () => {
    const { unmount } = render(createElement(Probe));

    const observer = FakeIntersectionObserver.latest();
    expect(observer.disconnected).toBe(false);

    unmount();
    expect(observer.disconnected).toBe(true);
  });
});
