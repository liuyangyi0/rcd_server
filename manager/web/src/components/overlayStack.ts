// Open drawers and dialogs. Only the innermost one answers Escape (and, for a dialog, traps
// Tab), so one key press never closes a drawer behind the dialog it opened. A dialog always
// outranks a drawer, whatever order their effects ran in; within a layer the latest is on top.
type Layer = 'drawer' | 'dialog';
const stack: { token: symbol; layer: Layer }[] = [];

export function pushOverlay(layer: Layer): symbol {
  const token = Symbol(layer);
  stack.push({ token, layer });
  return token;
}

export function popOverlay(token: symbol): void {
  const index = stack.findIndex((entry) => entry.token === token);
  if (index >= 0) stack.splice(index, 1);
}

export function isTopOverlay(token: symbol): boolean {
  const dialogs = stack.filter((entry) => entry.layer === 'dialog');
  const layer = dialogs.length ? dialogs : stack;
  return layer[layer.length - 1]?.token === token;
}
