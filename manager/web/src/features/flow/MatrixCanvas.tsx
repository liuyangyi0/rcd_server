import { MatrixScene, type MatrixState } from './MatrixScene';
import { useSceneCanvas } from './sceneCanvas';

export default function MatrixCanvas(props: MatrixState) {
  const canvas = useSceneCanvas(() => new MatrixScene(), props);
  return <canvas ref={canvas} className="flow-canvas" aria-hidden="true" />;
}
