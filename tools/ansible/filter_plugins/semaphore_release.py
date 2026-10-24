"""Resolve UI inputs against a controller-owned package/target allowlist."""
import re


READ_ONLY = {'preflight', 'history', 'verify-running', 'inspect-deployment'}
RECOVERY = {'resume-deployment', 'finish-deployment', 'rollback-deployment',
            'resume-rollback', 'finish-rollback'}


def identifier(value, field):
    if not isinstance(value, str) or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,95}', value):
        raise ValueError(field + ': 必须填写 1–96 位字母、数字、点、横线或下划线')
    return value


def resolve(values, catalog, host, workflow):
    component = values.get('component', 'lower')
    target = catalog['targets'][host]
    if component not in target['components']:
        raise ValueError('该目标没有所选组件')
    if workflow not in READ_ONLY | RECOVERY | {'stage', 'deploy', 'rollback', 'recover'}:
        raise ValueError('未知发布流程')
    action = values.get('recovery_action', '') if workflow == 'recover' else workflow
    if workflow == 'recover' and action not in RECOVERY | {'inspect-deployment'}:
        raise ValueError('请选择明确的恢复阶段')
    execution = values.get('execution', 'preview')
    if execution not in ('preview', 'execute'):
        raise ValueError('执行方式非法')
    mode = values.get('mode', 'manual')
    if mode not in ('manual', 'maintenance'):
        raise ValueError('发布模式非法')
    if action in RECOVERY | {'inspect-deployment'}:
        mode = 'maintenance'
    if mode == 'maintenance' and component != 'lower':
        raise ValueError('维护事务仅支持下级组件；管理端和 Agent 使用人工暂停模式')
    if action == 'rollback':
        mode = 'manual'
    root = target['components'][component]['release_dir']
    release = {'host': host, 'component': component,
               'profile': target['components'][component]['profile'],
               'tool_dir': '/var/lib/rcd-release/tools', 'mode': mode}
    if workflow in ('stage', 'deploy', 'verify-running'):
        version = values.get('release_id', '')
        package = catalog['packages'][version][component]
        release.update(controller_artifact=package['path'], manifest_sha256=package['manifest_sha256'],
                       incoming_dir=root + '/incoming/' + version,
                       artifact_dir=root + '/artifacts/' + version)
    if action == 'rollback':
        # Relative reference copied from the history task, never arbitrary paths.
        ref = values.get('backup_ref', '')
        if ref.startswith(root + '/'):
            ref = ref[len(root) + 1:]
        if not re.fullmatch(r'artifacts/[A-Za-z0-9][A-Za-z0-9_.-]*/backup-[a-f0-9]{32}', ref):
            raise ValueError('请从原升级任务日志复制 recovery_backup 备份路径')
        release['artifact_dir'] = root + '/' + ref
        if values.get('recover_operation_id'):
            release['recover_operation_id'] = identifier(values['recover_operation_id'], '原操作编号')
    if mode == 'maintenance':
        release['deployment_id'] = identifier(values.get('deployment_id', ''), '维护事务编号')
    mutate = action not in READ_ONLY | {'stage'}
    if mutate and execution == 'execute':
        if values.get('command_pause') != 'confirmed':
            raise ValueError('尚未确认所有管理端和直连客户端已暂停控制命令')
        release['operation_id'] = identifier(values.get('operation_id', ''), '本次操作编号')
        if not isinstance(values.get('initiator'), str) or not values['initiator'].strip():
            raise ValueError('须填写操作者')
    restore = values.get('restore_data', 'keep')
    if restore not in ('keep', 'restore') or (restore == 'restore' and action != 'rollback'):
        raise ValueError('数据恢复仅允许用于明确的备份回滚')
    return {'release': release, 'action': action,
            'preview': mutate and execution == 'preview',
            'restore_data': restore == 'restore', 'initiator': values.get('initiator', ''),
            'no_active_commands': mutate and execution == 'execute',
            'confirm_targets': [host + '/' + component] if mutate and execution == 'execute' else []}


class FilterModule:
    def filters(self):
        return {'rcd_semaphore_release': resolve}
