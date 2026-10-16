import { useEffect, useRef, useState } from 'react';

// 默认阈值 0.6:面板有六成压在视口里才算"在看它"。
// 定这么高是因为抖音式首页一屏只该有一个视频在播,阈值太低会让相邻两屏的边缘
// 互相争夺播放权,出现"两个视频同时出声"。
const DEFAULT_THRESHOLD = 0.6;

// 判断元素是否进入视口的通用 hook,返回 [ref, 是否可见]。
//
// 存在的理由:首页会一次性挂上 N 个 <video>(每屏一个),但机器同时播十几个视频会卡;
// 只有视口里那个可以播,其余必须暂停 —— 这里提供"谁在视口里"这一个事实,播不播由调用方决定。
// 交给 IntersectionObserver 而不是手写 scroll 监听:后者要在滚动事件里反复 getBoundingClientRect,
// 每帧都强制同步布局,长列表下必卡;IntersectionObserver 由浏览器在合成线程上算,零主线程开销。
// 同样地,Task 8 的"触底加载"哨兵元素也复用这个 hook —— 它只需要"元素进来没有",不关心是视频还是哨兵。
export function useInView<T extends Element>(
  opts?: { threshold?: number },
): [React.RefObject<T>, boolean] {
  const threshold = opts?.threshold ?? DEFAULT_THRESHOLD;
  const ref = useRef<T>(null);
  const [inView, setInView] = useState(false);

  useEffect(() => {
    const el = ref.current;
    // ref 还没挂上,或环境没有 IntersectionObserver(老浏览器 / 非浏览器),就保持"不可见"。
    // 宁可什么都不播,也不抛错把整个首页炸掉。
    if (!el || typeof IntersectionObserver === 'undefined') return;

    // 每次回调只取第一条 entry:hook 只 observe 一个元素,数组里必定只有它。
    const observer = new IntersectionObserver(([entry]) => setInView(entry.isIntersecting), {
      threshold,
    });
    observer.observe(el);

    // 卸载时断开:元素已经从 DOM 上摘掉了,再留着一个 observer 盯着它既是内存泄漏,
    // 回调进来还会对已卸载的组件 setState。
    return () => observer.disconnect();
  }, [threshold]);

  return [ref, inView];
}
