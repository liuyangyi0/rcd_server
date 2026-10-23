#!/usr/bin/env python3
"""Create RCD read-only templates through the documented Semaphore API.

Run on the controller as root. RCD_NODE_PASSWORD is consumed from the environment,
encrypted by Semaphore in its key store, never written into playbooks or output.
Does not run tasks or create schedules/deploy templates.
"""
import http.cookiejar
import json
import os
from pathlib import Path
import urllib.request


def main():
    config = json.loads(Path('/etc/semaphore/config.json').read_text())
    base = config['web_host'] + '/api'
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}),
        urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))

    def api(path, payload=None):
        request = urllib.request.Request(base + path,
            data=json.dumps(payload).encode() if payload is not None else None,
            headers={'Content-Type': 'application/json'})
        with opener.open(request, timeout=20) as response:
            raw = response.read()
            return json.loads(raw) if raw else None

    api('/auth/login', {'auth': 'admin', 'password': os.environ.get('SEMAPHORE_ADMIN_PASSWORD') or Path('/etc/semaphore/initial-admin-password').read_text().strip()})
    password = os.environ.get('RCD_NODE_PASSWORD')
    if not password:
        raise RuntimeError('RCD_NODE_PASSWORD must be supplied securely for initial key-store setup')
    projects = api('/projects')
    project = next((p for p in projects if p['name'] == 'RCD 运维'), None)
    if project is None:
        project = api('/projects', {'name': 'RCD 运维', 'alert': False, 'max_parallel_tasks': 1})
    prefix = '/project/' + str(project['id'])

    def ensure(path, body):
        existing = next((x for x in api(prefix + path) if x['name'] == body['name']), None)
        return existing or api(prefix + path, dict(body, project_id=project['id']))

    none = ensure('/keys', {'name': '本地只读脚本（无需仓库凭据）', 'type': 'none'})
    repository = ensure('/repositories', {'name': 'RCD 已验证运维脚本', 'git_url': '/opt/semaphore/playbooks',
                                         'git_branch': 'snapshot', 'ssh_key_id': none['id']})
    environment = ensure('/environment', {'name': '只读巡检（不含部署授权）', 'json': '{}',
        'env': json.dumps({'ANSIBLE_HOST_KEY_CHECKING': 'True', 'ANSIBLE_PARAMIKO_HOST_KEY_CHECKING': 'True',
                           'ANSIBLE_PARAMIKO_LOOK_FOR_KEYS': 'False'})})
    definitions = [
        ('30', 'RCD93', [{'component': 'lower-stack', 'instance': 'rcd93-30', 'scope': 'system', 'unit': 'rcd-lower.service'},
                         {'component': 'manager', 'instance': 'main', 'scope': 'user', 'unit': 'rcd-manager.service'}]),
        ('46', 'lenovo', [{'component': 'lower-stack', 'instance': 'real-lower', 'scope': 'user', 'unit': 'rcd-process-agent@real-lower.service'},
                         {'component': 'manager', 'instance': 'main', 'scope': 'user', 'unit': 'rcd-manager.service'}]),
        ('58', 'lenovo', [{'component': 'lower-stack', 'instance': 'rcd92-58', 'scope': 'system', 'unit': 'rcd-lower.service'},
                         {'component': 'manager', 'instance': 'main', 'scope': 'user', 'unit': 'rcd-manager.service'}])]
    templates = []
    for suffix, login, services in definitions:
        key = ensure('/keys', {'name': 'RCD SSH ' + login, 'type': 'login_password',
                              'login_password': {'login': login, 'password': password}})
        alias = 'rcd' + suffix
        inventory_body = {'all': {'children': {'rcd_nodes': {'hosts': {alias: {
            'ansible_host': '192.168.10.' + suffix, 'ansible_user': login,
            'ansible_connection': 'paramiko', 'ansible_python_interpreter': '/usr/bin/python3',
            'rcd_services': services}}}}}}
        inventory = ensure('/inventory', {'name': 'RCD ' + suffix, 'type': 'static-yaml',
            'inventory': json.dumps(inventory_body), 'ssh_key_id': key['id']})
        template = ensure('/templates', {'name': suffix + '｜软件与服务巡检（只读）', 'app': 'ansible',
            'playbook': 'tools/ansible/inspect.yml', 'inventory_id': inventory['id'],
            'repository_id': repository['id'], 'environment_id': environment['id'],
            'arguments': json.dumps(['--limit', alias]), 'allow_override_args_in_task': False,
            'description': '只读检查版本、固定路径、服务状态。不会部署、重启或发送设备命令。'})
        templates.append({'id': template['id'], 'name': template['name'], 'host': '192.168.10.' + suffix})
    report = {'project_id': project['id'], 'templates': templates, 'schedules_created': False,
              'live_tasks_executed': False, 'mutating_templates_created': False}
    path = Path('/etc/semaphore/project-bootstrap.json')
    path.write_text(json.dumps(report, ensure_ascii=False, indent=2))
    path.chmod(0o600)
    print(json.dumps(report, ensure_ascii=False))


if __name__ == '__main__':
    main()
