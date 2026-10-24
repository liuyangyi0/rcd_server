import type { DeviceGroupBinding, Source } from '../api/types';

/** The physical lower connection may supply several independently routed groups. */
export function sourceHasGroup(source: Pick<Source, 'groupId' | 'groupBindings'>, groupId: string): boolean {
  return source.groupId === groupId || Boolean(source.groupBindings?.some((binding) => binding.groupId === groupId));
}

export function sourceGroupIds(source: Pick<Source, 'groupId' | 'groupBindings'>): string[] {
  return [...new Set([source.groupId, ...(source.groupBindings ?? []).map((binding) => binding.groupId)])].filter(Boolean);
}

export function sourceUnavailableReason(source: Source, groupId: string): string | undefined {
  if (!sourceHasGroup(source, groupId)) return '非本组来源';
  if (!source.enabled) return '来源已停用';
  if (!['ready', 'cached'].includes(source.catalogState.toLowerCase())) return source.catalogState || '目录未就绪';
  if (source.availableGroupIds !== undefined && !source.availableGroupIds.includes(groupId)) return '未发现本组目录';
  return undefined;
}

export function sourceSelectableForGroup(source: Source, groupId: string): boolean {
  return sourceUnavailableReason(source, groupId) === undefined;
}

export function deviceKey(portId: string, deviceId: string): string {
  return JSON.stringify([portId, deviceId]);
}

export function sameBindings(left: DeviceGroupBinding[], right: DeviceGroupBinding[]): boolean {
  const stable = (bindings: DeviceGroupBinding[]) => bindings.map((binding) => JSON.stringify([binding.portId, binding.deviceId, binding.groupId])).sort();
  return JSON.stringify(stable(left)) === JSON.stringify(stable(right));
}

export function groupDeviceLabel(source: Source, groupId: string): string {
  const devices = (source.groupBindings ?? []).filter((binding) => binding.groupId === groupId);
  const labels = devices.map((binding) => `${binding.portId} / 设备 ${binding.deviceId}`);
  if (source.groupId === groupId) labels.unshift('未映射设备 / 跨设备计算 / 共享命令');
  return labels.join('；');
}
