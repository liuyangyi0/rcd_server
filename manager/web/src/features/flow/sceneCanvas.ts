import { useEffect, useRef } from 'react';
import type { SceneFrame } from './sceneKit';

export interface CanvasScene<S> { draw(ctx: CanvasRenderingContext2D, state: S, frame: SceneFrame): void }

// Shared render loop for the flow scenes; the scene is created once per mounted canvas.
export function useSceneCanvas<S extends { paused: boolean }>(create: () => CanvasScene<S>, props: S) {
  const canvas = useRef<HTMLCanvasElement>(null), latest = useRef(props), factory = useRef(create);
  latest.current = props;
  useEffect(() => {
    const element = canvas.current, ctx = element?.getContext('2d');
    if (!element || !ctx) return;
    const scene = factory.current(), reducedMotion = window.matchMedia('(prefers-reduced-motion: reduce)');
    let frame = 0, last = 0, width = 0, height = 0, cost = 4, interval = 1000 / 60, shown: S | undefined;
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
  return canvas;
}
