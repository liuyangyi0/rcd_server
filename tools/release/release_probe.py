#!/usr/bin/env python3
"""Read-only release evidence. Safe to run from the native inspection task.

This file is standalone for Ansible script transport. It never imports a
publisher, executes a binary, acquires a release, or changes business state.
"""
import argparse
from contextlib import closing
import errno
import hashlib
import json
from pathlib import Path
import re
import sqlite3
from application_profile import application_slug, validate_application_profile

ADAPTER_VERSION = '1.1.0'


class ReleaseProblem(RuntimeError):
    def __init__(self, code, message):
        super().__init__(message)
        self.code, self.public_message = code, message


def public_error(error, stage=''):
    if isinstance(error, ReleaseProblem):
        return {'code': error.code, 'message': error.public_message}
    number = getattr(error, 'errno', None)
    if number == errno.ENOSPC:
        return {'code': 'disk_full', 'message': '磁盘空间不足，请释放空间后检查原事务，勿重复发布'}
    if number in (errno.EACCES, errno.EPERM):
        return {'code': 'permission_denied', 'message': '文件或服务访问被拒绝，请检查权限和 KYSEC 策略'}
    if number == errno.EROFS:
        return {'code': 'read_only_storage', 'message': '存储为只读，无法完成备份或替换'}
    if isinstance(error, (TimeoutError, ConnectionError)):
        return {'code': 'response_unknown', 'message': '连接或响应超时；请补查原事务，不会自动重复执行'}
    labels = {'maintenance': '维护准入或事务排空', 'stopped': '停止程序',
              'backed_up': '备份', 'installed': '替换程序',
              'software_acceptance': '启动及运行映像验收',
              'maintenance_completed': '维护收尾', 'automatic_binary_recovery': '恢复原程序'}
    return {'code': 'release_check_failed',
            'message': (labels.get(stage, '发布前检查') + '未通过，请查看该阶段和原任务日志；未确认的操作不会重投')}


def read_json(path, limit=262144):
    path = Path(path)
    if path.is_symlink() or not path.is_file() or path.stat().st_size > limit:
        raise ReleaseProblem('invalid_evidence', '状态文件缺失、超限或不是普通文件')
    with path.open('rb') as stream:
        raw = stream.read(limit + 1)
    if len(raw) > limit:
        raise ReleaseProblem('invalid_evidence', '状态文件超过允许大小')
    return json.loads(raw)


def read_formats(profile):
    component = profile['component']
    if component == 'communicate':
        if profile.get('databases'):
            raise ReleaseProblem('state_inventory_invalid', '通信程序未声明数据库，拒绝不明的数据绑定')
        return {}
    if application_slug(component):
        validate_application_profile(profile)
        return {}
    if component == 'agent':
        value = read_json(Path(profile['runtime_dir']) / 'process-state.json', 4 << 20)['schema_version']
        if type(value) is not int or value < 1:
            raise ReleaseProblem('state_inventory_invalid', 'Agent 状态格式不合法')
        return {'agent_state': value}
    result = {}
    for item in profile.get('databases', []):
        path = Path(item)
        name = 'manager_db' if component == 'manager' else {
            'command-journal.sqlite': 'command_journal', 'maintenance.sqlite': 'maintenance'}.get(path.name)
        if name is None:
            continue
        if name in result or path.is_symlink() or not path.is_file():
            raise ReleaseProblem('state_inventory_invalid', '数据格式检查的文件绑定不完整或重复')
        with closing(sqlite3.connect(path.as_uri() + '?mode=ro', uri=True)) as db:
            result[name] = db.execute('PRAGMA user_version').fetchone()[0]
    expected = {'manager': {'manager_db'}, 'lower': {'command_journal', 'maintenance'}}.get(component)
    if expected is None:
        raise ReleaseProblem('state_inventory_invalid', '未登记的软件类型')
    if set(result) != expected or any(type(v) is not int or v < 0 for v in result.values()):
        raise ReleaseProblem('state_inventory_invalid', '数据格式检查缺少必要数据库')
    return result


def probe(profiles, host, operation=None, applications=None, project_id=None):
    applications = [] if applications is None else applications
    if not isinstance(applications, list) or len(applications)>32:
        raise ValueError('bounded registered applications required')
    apps = {a['component']: a for a in applications if application_slug(a.get('component'))}
    if len(apps)!=len(applications):
        raise ValueError('unique registered applications required')
    profiles = dict(profiles)
    profiles.setdefault('communicate', '/etc/rcd/release-platform/communicate.json')
    for component in apps:
        profiles[component]='/etc/rcd/release-platform/'+component+'.json'
    compatibility = {}
    for component in ('lower', 'manager', 'agent', 'communicate', *sorted(apps)):
        try:
            profile = read_json(profiles[component])
            if profile.get('component') != component or profile.get('host') != host:
                raise ReleaseProblem('profile_binding_mismatch', '发布配置与目标机器或组件不一致')
            if component in apps and profile.get('application',{}).get('definition_sha256')!=apps[component].get('definition_sha256'):
                raise ReleaseProblem('profile_binding_mismatch', '软件定义与本机配置不一致')
            if component in apps and (type(project_id) is not int or project_id<1 or profile.get('application',{}).get('project_id')!=project_id):
                raise ReleaseProblem('profile_binding_mismatch', '该软件归属于另一个项目，不能升级或启停')
            compatibility[component] = {'state_formats': read_formats(profile),
                                        'adapter_version': ADAPTER_VERSION, 'error': ''}
            # Fixed, administrator-owned paths only; neither request arguments
            # nor browser claims can supply a replacement machine identity.
            try:
                if component == 'manager':
                    databases = profile.get('databases', [])
                    if len(databases) != 1:
                        raise ValueError('identity database binding is not unique')
                    path = Path(databases[0])
                    if path.is_symlink():
                        raise ValueError('identity database must not be a symlink')
                    with closing(sqlite3.connect(path.as_uri() + '?mode=ro', uri=True)) as db:
                        found = db.execute('SELECT instance_id FROM manager_identity WHERE id=1').fetchone()
                    if found and re.fullmatch('[A-Za-z0-9:_.-]{1,128}', found[0]):
                        compatibility[component]['instance_id'] = found[0]
                elif component == 'agent':
                    state = read_json(Path(profile['runtime_dir']) / 'process-state.json', 4 << 20)
                    lower = read_json(profile['lower_profile'])
                    if state['instance_id'] == lower['control']['instance_id']:
                        compatibility[component]['instance_id'] = state['instance_id']
                        compatibility[component]['gateway_instance_id'] = lower['gateway_instance_id']
            except Exception:
                # Legacy versions still have usable software/compatibility
                # evidence, but cannot establish a new physical binding.
                compatibility[component]['identity_error'] = '未取得可信实例身份，不能建立新台账绑定'
        except Exception as error:
            compatibility[component] = {'state_formats': {}, 'adapter_version': ADAPTER_VERSION,
                                        'error': public_error(error)['message']}
    report = None
    if operation:
        # Fixed profile resolves the only permitted report directory. A browser
        # never supplies a path or shell expression.
        try:
            if not re.fullmatch('[a-f0-9]{64}', operation['operation_id']):
                raise ValueError('invalid operation identity')
            profile = read_json(profiles[operation['component']])
            if profile.get('host') != host or profile.get('component') != operation['component']:
                raise ValueError('profile mismatch')
            path = Path(profile['release_dir']) / 'platform-reports' / (operation['operation_id'] + '.json')
            try:
                report = read_json(path)
            except (FileNotFoundError, ReleaseProblem):
                report = completed_journal_report(profile, operation)
            for key in ('operation_id', 'task_id', 'device_id', 'host_alias'):
                if report.get(key) != operation[key]:
                    raise ValueError('report identity mismatch')
            if report.get('schema_version') != 1:
                raise ValueError('report schema mismatch')
        except Exception:
            # Absent final evidence is unknown, never proof that nothing ran.
            report = None
    return {'schema_version': 1, 'compatibility': compatibility, 'release_report': report}


def completed_journal_report(profile, operation):
    """Only reconstruct a terminal result with all original immutable bindings.

    Missing/partial intent never becomes success or 'not started'. No journal or
    host marker is edited, including when the final report write was interrupted.
    """
    record = read_json(Path(profile['release_dir'])/'deployments'/operation['operation_id']/'deployment.json')
    if application_slug(profile.get('component')):
        validate_application_profile(profile)
        marker_dir=Path(profile['host_lock_dir'])
    else:
        marker_dir=Path(profile['executable']).parent
    marker = read_json(marker_dir/'.rcd-host-release-active.json')
    if marker.get('deployment_id') != operation['operation_id'] or marker.get('state') != record.get('state'):
        raise ValueError('host completion was not committed')
    identity = hashlib.sha256(json.dumps(profile,sort_keys=True,separators=(',', ':')).encode()).hexdigest()
    if (record.get('schema_version') != 1 or record.get('profile_sha256') != identity
            or record.get('deployment_id') != operation['operation_id']
            or record.get('component') != operation['component'] or record.get('host') != profile['host']):
        raise ValueError('journal identity mismatch')
    binding = record.get('report_binding') or {}
    for key in ('operation_id','task_id','device_id','host_alias'):
        if binding.get(key) != operation[key]: raise ValueError('journal task mismatch')
    stages = record.get('stages', [])
    if not stages or stages[-1].get('state') != 'confirmed': raise ValueError('incomplete journal')
    if record.get('kind') == 'service_control' and record.get('state') == 'succeeded':
        if application_slug(profile.get('component')):
            expected_stage = {'start':'application_start', 'stop':'application_stop', 'restart':'application_start'}.get(record.get('action'))
        else:
            expected_stage = {'start':'service_start', 'stop':'service_stop', 'restart':'service_start', 'recover':'agent_recovery'}.get(record.get('action'))
        acceptance = record.get('acceptance', {})
        wanted = 'stopped' if record.get('action') == 'stop' else 'running'
        if (stages[-1].get('stage') != expected_stage or acceptance.get('state') != wanted
                or acceptance.get('binary_sha256') != record['binary_sha256']):
            raise ValueError('incomplete service acceptance')
    elif record.get('state') == 'succeeded' and stages[-1].get('stage') == 'maintenance_completed':
        accepted = [s['result'] for s in stages if s.get('stage') == 'software_acceptance' and s.get('state') == 'confirmed']
        if len(accepted) != 1: raise ValueError('missing acceptance')
        acceptance = accepted[0]
        if acceptance.get('binary_sha256') != record['binary_sha256']: raise ValueError('wrong image')
    elif record.get('state') == 'recovered' and stages[-1].get('stage') == 'automatic_binary_recovery':
        acceptance = stages[-1]['result']['acceptance']
        if acceptance.get('binary_sha256') != record['previous_binary_sha256']: raise ValueError('wrong recovered image')
    else:
        raise ValueError('not safely completed')
    public_fields = ('stage','state','started_at','ended_at','error_type','error')
    return dict(binding,schema_version=1,state=record['state'],error='',binary_sha256=acceptance['binary_sha256'],
                acceptance=acceptance,stages=[{k:v for k,v in s.items() if k in public_fields} for s in stages])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--profiles', required=True)
    parser.add_argument('--host', required=True)
    parser.add_argument('--operation', default='null')
    parser.add_argument('--applications', default='[]')
    parser.add_argument('--project-id', type=int)
    args = parser.parse_args()
    print(json.dumps(probe(json.loads(args.profiles), args.host, json.loads(args.operation), json.loads(args.applications), args.project_id), ensure_ascii=False))


if __name__ == '__main__':
    main()
