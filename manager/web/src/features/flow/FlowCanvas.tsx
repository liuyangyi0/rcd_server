import { useEffect, useRef } from 'react';
import { FlowScene, type SceneState } from './ParticleScene';
export { laneY } from './ParticleScene';

export default function FlowCanvas(props: SceneState) {
  const canvas = useRef<HTMLCanvasElement>(null), latest = useRef(props);
  latest.current = props;
  useEffect(() => {
    const element = canvas.current, ctx = element?.getContext('2d');
    if (!element || !ctx) return;
    const scene = new FlowScene(), reducedMotion = window.matchMedia('(prefers-reduced-motion: reduce)');
    let frame = 0, last = 0, width = 0, height = 0, cost = 4, interval = 1000 / 60, shown: SceneState | undefined;
    const resize = () => {
      const rect = element.getBoundingClientRect(), dpr = Math.min(window.devicePixelRatio || 1, 2);
      width = element.width = Math.round(rect.width * dpr); height = element.height = Math.round(rect.height * dpr); shown = undefined;
    };
    const observer = new ResizeObserver(resize); observer.observe(element); resize();
    function draw(now: number) {
      frame = requestAnimationFrame(draw);
      if (document.hidden || !width || !height || now - last < interval - 2) return;
      const state = latest.current, motion = !state.paused && !reducedMotion.matches;
      // Still frames only change with data, size or source recency.
      if (!motion && state === shown && now - last < 500) return;
      const started = performance.now();
      scene.draw(ctx!, state, { width, height, dt: motion && last ? Math.min(now - last, 100) / 1000 : 0, motion, now });
      // Slow display hosts drop to 30 fps instead of stuttering.
      cost = cost * .9 + (performance.now() - started) * .1; interval = cost > 10 ? 1000 / 30 : cost < 6 ? 1000 / 60 : interval;
      last = now; shown = state;
    }
    frame = requestAnimationFrame(draw);
    return () => { cancelAnimationFrame(frame); observer.disconnect(); };
  }, []);
  return <canvas ref={canvas} className="flow-canvas" aria-hidden="true" />;
}
