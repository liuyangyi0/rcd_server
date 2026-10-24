import { useEffect, useRef } from 'react';
import { drawScene, makeParticleSprite, SCENE_HEIGHT, SCENE_WIDTH, type SceneState } from './ParticleScene';
export { laneY } from './ParticleScene';

export default function FlowCanvas(props: SceneState) {
  const canvas = useRef<HTMLCanvasElement>(null), latest = useRef(props);
  latest.current = props;
  useEffect(() => {
    const element = canvas.current, ctx = element?.getContext('2d');
    if (!element || !ctx) return;
    const sprite = makeParticleSprite();
    let frame = 0, last = 0, seconds = 0, width = 0, height = 0, dpr = 1;
    const reducedMotion = window.matchMedia('(prefers-reduced-motion: reduce)');
    const resize = () => {
      const rect = element.getBoundingClientRect();
      width = rect.width; height = rect.height; dpr = Math.min(window.devicePixelRatio || 1, 2);
      element.width = Math.round(width * dpr); element.height = Math.round(height * dpr);
    };
    const observer = new ResizeObserver(resize); observer.observe(element); resize();
    function draw(now: number) {
      frame = requestAnimationFrame(draw);
      if (document.hidden || now - last < 1000 / 30 || !width || !height) return;
      if (!latest.current.paused && !reducedMotion.matches) seconds += Math.min(now - last, 100) / 1000;
      last = now;
      ctx!.setTransform(width * dpr / SCENE_WIDTH, 0, 0, height * dpr / SCENE_HEIGHT, 0, 0);
      drawScene(ctx!, latest.current, seconds, now, sprite);
    }
    frame = requestAnimationFrame(draw);
    return () => { cancelAnimationFrame(frame); observer.disconnect(); };
  }, []);
  return <canvas ref={canvas} className="flow-canvas" aria-hidden="true" />;
}
