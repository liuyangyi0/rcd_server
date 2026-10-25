import { FlowScene, type SceneState } from './ParticleScene';
import { useSceneCanvas } from './sceneCanvas';
export { laneY } from './ParticleScene';

export default function FlowCanvas(props: SceneState) {
  const canvas = useSceneCanvas(() => new FlowScene(), props);
  return <canvas ref={canvas} className="flow-canvas" aria-hidden="true" />;
}
