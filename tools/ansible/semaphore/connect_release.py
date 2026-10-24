#!/usr/bin/env python3
"""Connect release workflows. Never starts a task or controls a live service.

Run prepare-target ON each named host as root; export/register on controller 46.
Credentials are environment-only. Package export verifies the recorded release.
"""
import argparse
import hashlib
import http.cookiejar
import json
import os
from pathlib import Path
import re
import shutil
import sqlite3
import subprocess
import tempfile
import urllib.request

# Second values are stable Agent instance IDs, not the OPC rcd-30/46/58 source IDs.
TARGETS = {'30': ('RCD93', 'rcd93-30'), '46': ('lenovo', 'real-lower'), '58': ('lenovo', 'rcd92-58')}
COMPONENTS = {'lower': 'rcd_server', 'agent': 'RcdAgent', 'manager': 'RcdManager'}
RELEASE = 'rcd-standard-paths-20260928'


def target_components(suffix):
    if suffix not in TARGETS:
        raise ValueError('unknown RCD target')
    return list(COMPONENTS)


def write(path, data, mode=0o644):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix('.tmp')
    temporary.write_text(json.dumps(data, ensure_ascii=False, indent=2), encoding='utf-8')
    temporary.chmod(mode)
    os.replace(temporary, path)


def digest(path):
    h = hashlib.sha256()
    with Path(path).open('rb') as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b''):
            h.update(chunk)
    return h.hexdigest()


def prepare_target(suffix):
    host = '192.168.10.' + suffix
    if host not in subprocess.check_output(['hostname', '-I'], text=True).split():
        raise RuntimeError('must run ON the named target')
    login, instance = TARGETS[suffix]
    components = target_components(suffix)
    result = {}
    for component in components:
        runtime = '/var/lib/rcd/' + ('lower/' + instance if component == 'lower' else
                                   'agent/' + instance if component == 'agent' else 'manager/main')
        control = ({'kind': 'process-agent', 'instance_id': instance, 'url': 'http://127.0.0.1:8090'}
                   if component == 'lower' else
                   {'kind': 'systemd-user', 'run_as': login, 'unit': 'rcd-manager.service'}
                   if component == 'manager' else
                   {'kind': 'systemd-user', 'run_as': login, 'unit': 'rcd-process-agent@' + instance + '.service'}
                   if suffix == '46' else {'kind': 'systemd-system', 'unit': 'rcd-lower.service'})
        profile = {'host': host, 'component': component, 'source_dir': '/var/lib/rcd-build/source',
                   'release_dir': '/var/lib/rcd-release/' + component,
                   'executable': '/opt/rcd/bin/' + COMPONENTS[component], 'runtime_dir': runtime,
                   'control': control, 'opc_port': 4840 if component == 'manager' else 4841,
                   'config_files': [], 'databases': []}
        if component == 'lower':
            profile['config_dirs'] = [runtime + '/config', runtime + '/pki']
            profile['config_files'] = [runtime + '/state/' + name for name in
                                       ('operations.json', 'station_versions.json') if Path(runtime + '/state/' + name).is_file()]
            profile['databases'] = [str(p) for p in sorted((Path(runtime) / 'state').glob('*.sqlite'))]
        elif component == 'agent':
            profile['config_files'] = [runtime + '/process-state.json']
            profile['health_url'] = 'http://127.0.0.1:8090/healthz'
        else:
            profile['databases'] = [runtime + '/manager.db']
            profile['config_dirs'] = [runtime + '/pki']
            profile['health_url'] = 'http://127.0.0.1:8087/healthz'
        path = '/etc/rcd/release-profiles/' + component + '.json'
        # Never silently change binding for staged packages or unfinished jobs.
        if Path(path).exists() and json.loads(Path(path).read_text()) != profile:
            raise RuntimeError('existing profile differs: review explicitly before replacement: ' + path)
        write(path, profile)
        Path(profile['release_dir']).mkdir(parents=True, exist_ok=True, mode=0o700)
        result[component] = {'profile': path, 'release_dir': profile['release_dir']}
    print(json.dumps({'host': host, 'components': result, 'services_changed': False}))


def export_catalog():
    manifest_path = Path('/opt/rcd/releases') / RELEASE / 'manifest.json'
    recorded = json.loads(manifest_path.read_text())
    packages = {}
    for component, binary in COMPONENTS.items():
        entry = recorded['components'][binary]
        source = Path('/opt/rcd/bin') / binary
        if digest(source) != entry['sha256']:
            raise RuntimeError('current binary differs from recorded package: ' + binary)
        directory = Path('/opt/semaphore/artifacts') / RELEASE / component
        manifest = {'host': '192.168.10.46', 'component': component, 'build_info': entry['build_info'],
                    'binary_sha256': entry['sha256'], 'source': recorded['source'],
                    'declared_base_commit': recorded['identity']['commit'],
                    'exported_from_manifest_sha256': digest(manifest_path)}
        if directory.exists():
            if json.loads((directory / 'manifest.json').read_text()) != manifest or digest(directory / 'binary') != entry['sha256']:
                raise RuntimeError('immutable catalog package conflict')
        else:
            directory.mkdir(parents=True, mode=0o755)
            shutil.copyfile(source, directory / 'binary')
            (directory / 'binary').chmod(0o644)
            write(directory / 'manifest.json', manifest)
        packages[component] = {'path': str(directory), 'manifest_sha256': digest(directory / 'manifest.json')}
    targets = {}
    for suffix in TARGETS:
        components = target_components(suffix)
        targets['192.168.10.' + suffix] = {'components': {c: {
            'profile': '/etc/rcd/release-profiles/' + c + '.json', 'release_dir': '/var/lib/rcd-release/' + c} for c in components}}
    write('/opt/semaphore/catalog/releases.json', {'schema_version': 1, 'targets': targets, 'packages': {RELEASE: packages}})
    print(json.dumps({'catalog': '/opt/semaphore/catalog/releases.json', 'release_id': RELEASE,
                      'note': 'recorded current release for reinstall; no service replacement'}))


def connection():
    password = os.environ.get('SEMAPHORE_ADMIN_PASSWORD')
    if not password:
        raise RuntimeError('SEMAPHORE_ADMIN_PASSWORD required; initial password may have been rotated')
    config = json.loads(Path('/etc/semaphore/config.json').read_text())
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}),
        urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))

    def api(path, body=None, method=None):
        req = urllib.request.Request(config['web_host'] + '/api' + path,
            data=json.dumps(body).encode() if body is not None else None, method=method,
            headers={'Content-Type': 'application/json'})
        with opener.open(req, timeout=30) as response:
            raw = response.read()
            return json.loads(raw) if raw else None
    api('/auth/login', {'auth': 'admin', 'password': password})
    return api


def survey(name, title, choices=None, default='', required=False, description=''):
    result = {'name': name, 'title': title, 'type': 'enum' if choices else '', 'required': required,
              'description': description, 'default_value': default}
    if choices:
        result['values'] = [{'name': label, 'value': value} for value, label in choices]
    return result


def register():
    api = connection()
    project = next(p for p in api('/projects') if p['name'] == 'RCD 运维')
    prefix = '/project/' + str(project['id'])
    repository = next(x for x in api(prefix + '/repositories') if x['name'] == 'RCD 已验证运维脚本')
    environments = api(prefix + '/environment')
    environment = next((x for x in environments if x['name'] == '软件维护（执行需确认）'), None)
    if environment is None:
        environment = api(prefix + '/environment', {'project_id': project['id'], 'name': '软件维护（执行需确认）',
            'json': '{}', 'env': json.dumps({'ANSIBLE_HOST_KEY_CHECKING': 'True',
                'ANSIBLE_PARAMIKO_HOST_KEY_CHECKING': 'True', 'ANSIBLE_PARAMIKO_LOOK_FOR_KEYS': 'False'})})
    inventories = api(prefix + '/inventory')
    keys = api(prefix + '/keys')
    existing = api(prefix + '/templates')
    templates = []
    # Daily UI exposes outcomes, not every internal release transaction stage.
    # Inspection and task history already exist in Semaphore.
    workflows = [('deploy', '软件升级'), ('rollback', '版本回退')]
    for suffix, (login, _) in TARGETS.items():
        original = next(x for x in inventories if x['name'] == 'RCD ' + suffix)
        key = next(x for x in keys if x['name'] == 'RCD SSH ' + login)
        become_name = 'RCD sudo ' + login + ' → root'
        become = next((x for x in keys if x['name'] == become_name), None)
        if become is None:
            password = os.environ.get('RCD_NODE_PASSWORD')
            if not password:
                raise RuntimeError('RCD_NODE_PASSWORD required to initialize sudo credential')
            become = api(prefix + '/keys', {'project_id': project['id'], 'name': become_name,
                'type': 'login_password', 'login_password': {'login': 'root', 'password': password}})
            keys.append(become)
        # Separate inventory makes elevation explicit and leaves inspection unchanged.
        name = 'RCD ' + suffix + ' 发布（显式提权）'
        inv = next((x for x in inventories if x['name'] == name), None)
        detail = api(prefix + '/inventory/' + str(original['id']))
        content = json.loads(detail['inventory'])
        node = content['all']['children']['rcd_nodes']['hosts']['rcd' + suffix]
        node.update(ansible_become=True, ansible_become_user='root', ansible_become_method='sudo')
        body = {'project_id': project['id'], 'name': name, 'type': 'static-yaml',
                'inventory': json.dumps(content), 'ssh_key_id': key['id'], 'become_key_id': become['id']}
        if inv is None:
            inv = api(prefix + '/inventory', body)
        else:
            api(prefix + '/inventory/' + str(inv['id']), dict(body, id=inv['id']), 'PUT')
        for workflow, title in workflows:
            variables = [survey('component', '目标组件', [(c, {'lower': '下级采集 RCD', 'agent': '进程 Agent（会同时重启下级）',
                                'manager': 'OPC 管理端'}[c]) for c in target_components(suffix)], 'lower', True)]
            if workflow == 'deploy':
                catalog = json.loads(Path('/opt/semaphore/catalog/releases.json').read_text())
                variables.append(survey('release_id', '软件包版本', [(x, x + ('（当前现网版本，可重装）' if x == RELEASE else ''))
                    for x in catalog['packages']], RELEASE, True))
            if workflow in ('deploy', 'rollback'):
                variables += [survey('execution', '执行方式', [('preview', '仅检查，不重启'), ('execute', '确认执行（会重启所选程序）')], 'preview', True),
                    survey('command_pause', '全部控制命令已暂停', [('not_confirmed', '未确认'), ('confirmed', '已暂停 30、46、58 管理端及直连客户端的命令')], 'not_confirmed', True),
                ]
            if workflow == 'rollback':
                variables += [survey('backup_ref', '备份路径', required=True, description='从原升级任务日志复制 recovery_backup 的完整路径；保留当前配置和数据'),
                    ]
            body = {'project_id': project['id'], 'name': suffix + '｜' + title, 'app': 'ansible',
                    'playbook': 'tools/ansible/semaphore-release.yml', 'inventory_id': inv['id'],
                    'repository_id': repository['id'], 'environment_id': environment['id'],
                    'arguments': json.dumps(['--limit', 'rcd' + suffix, '--extra-vars', json.dumps({'rcd_workflow': workflow})]),
                    'allow_override_args_in_task': False, 'survey_vars': variables,
                    'description': '目标 192.168.10.' + suffix + '。自动完成检查、备份、替换、重启和验收；默认保留现场配置。操作详情在任务日志查看。异常结果不会自动重试。'}
            saved = next((t for t in existing if t['name'] == body['name']), None)
            if saved:
                body['id'] = saved['id']
                # Retain native UI grouping when refreshing execution definitions.
                body['view_id'] = saved.get('view_id')
                api(prefix + '/templates/' + str(saved['id']), body, 'PUT')
            else:
                saved = api(prefix + '/templates', body)
            templates.append({'id': saved['id'], 'name': body['name'], 'workflow': workflow, 'host': '192.168.10.' + suffix})
    report = {'project_id': project['id'], 'templates': templates, 'live_tasks_executed': False, 'schedules_created': False}
    write('/etc/semaphore/release-bootstrap.json', report, 0o600)
    print(json.dumps(report, ensure_ascii=False))


def install_snapshot(source):
    api = connection()
    project = next(p for p in api('/projects') if p['name'] == 'RCD 运维')
    if any(t['status'] in ('waiting', 'starting', 'running') for t in api('/project/' + str(project['id']) + '/tasks')):
        raise RuntimeError('wait for all current platform tasks before updating playbooks')
    root = Path('/var/lib/semaphore/backups')
    root.mkdir(mode=0o700, exist_ok=True)
    backup = Path(tempfile.mkdtemp(prefix='release-connection-', dir=root))
    with sqlite3.connect('file:/var/lib/semaphore/semaphore.sqlite?mode=ro', uri=True) as src, sqlite3.connect(backup / 'semaphore.sqlite') as dst:
        src.backup(dst)
        if dst.execute('PRAGMA quick_check').fetchone()[0] != 'ok':
            raise RuntimeError('platform backup failed')
    shutil.copytree('/opt/semaphore/playbooks/tools', backup / 'tools')
    for name in ('ansible', 'release'):
        shutil.copytree(Path(source) / 'tools' / name, Path('/opt/semaphore/playbooks/tools') / name,
                        dirs_exist_ok=True, ignore=shutil.ignore_patterns('__pycache__', '*.pyc'))
    files = {str(p.relative_to('/opt/semaphore/playbooks')): digest(p)
             for p in Path('/opt/semaphore/playbooks/tools').rglob('*') if p.is_file() and '__pycache__' not in p.parts}
    write('/opt/semaphore/release-tools-manifest.json', {'files': files, 'backup': str(backup), 'services_restarted': False})
    print(json.dumps({'snapshot_installed': True, 'backup': str(backup), 'services_restarted': False}))


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['prepare-target', 'export-catalog', 'register', 'install-snapshot'])
    parser.add_argument('--target', choices=TARGETS)
    parser.add_argument('--source', type=Path)
    args = parser.parse_args()
    if args.action == 'prepare-target':
        if not args.target:
            parser.error('--target is required')
        prepare_target(args.target)
    elif args.action == 'export-catalog':
        export_catalog()
    elif args.action == 'register':
        register()
    else:
        if not args.source or not args.source.is_absolute():
            parser.error('--source requires an explicit absolute directory')
        install_snapshot(args.source)
