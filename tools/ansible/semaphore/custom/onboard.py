#!/usr/bin/env python3
"""Bind the three existing hosts through native Semaphore APIs, without publishing.

Credentials are entered privately and stored only in the native encrypted key
store. This creates resources, but never changes an existing binding silently.
"""
import argparse
import getpass
import http.cookiejar
import json
import urllib.request

HOSTS = {'30': ('RCD93', 'rcd93-30'), '58': ('lenovo', 'rcd92-58'), '46': ('lenovo', 'real-lower')}


class Client:
    def __init__(self, base, login, password):
        self.base = base.rstrip('/')
        self.http = urllib.request.build_opener(urllib.request.ProxyHandler({}),
            urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
        self.request('/auth/login', {'auth': login, 'password': password})

    def request(self, path, data=None, method=None, headers=None):
        request = urllib.request.Request(self.base + '/api' + path,
            data=None if data is None else json.dumps(data).encode(), method=method,
            headers={'Content-Type': 'application/json', 'Origin': self.base, **(headers or {})})
        with self.http.open(request, timeout=45) as response:
            raw = response.read()
            return json.loads(raw) if raw else None


def inventory_hosts():
    hosts = {}
    for suffix, (login, instance) in HOSTS.items():
        hosts['rcd' + suffix] = {
            'ansible_host': '192.168.10.' + suffix, 'ansible_user': login,
            'ansible_connection': 'paramiko', 'ansible_python_interpreter': '/usr/bin/python3',
            'rcd_inspection_become': True,
            'rcd_release_profiles': {c: '/etc/rcd/release-platform/' + c + '.json' for c in ('lower','manager','agent')},
            'rcd_release_roots': ['/var/lib/rcd-release-platform/artifacts/' + c for c in ('lower','manager','agent')],
            'rcd_services': [
                {'component': 'manager', 'instance': 'main', 'scope': 'user', 'unit': 'rcd-manager.service', 'run_as': login},
                {'component': 'lower-stack', 'instance': instance, 'scope': 'user' if suffix == '46' else 'system',
                 'unit': 'rcd-process-agent@real-lower.service' if suffix == '46' else 'rcd-lower.service', 'run_as': login}]
        }
    return hosts


def register(api, project, repository, password, schedules, service_control=False):
    prefix = '/project/' + str(project)

    def ensure(path, body, stable=()):
        existing = next((x for x in api.request(prefix+path) if x['name'] == body['name']), None)
        if existing:
            for field in stable:
                if existing.get(field) != body.get(field):
                    raise RuntimeError('existing resource differs; review before changing: ' + body['name'] + '/' + field)
            return existing
        return api.request(prefix+path, dict(body, project_id=project))

    key = ensure('/keys', {'name': 'RCD 原生发布 SSH（用户名按机器）', 'type': 'login_password',
                           'login_password': {'login': '', 'password': password}}, ('type',))
    sudo = ensure('/keys', {'name': 'RCD 原生发布 sudo', 'type': 'login_password',
                            'login_password': {'login': 'root', 'password': password}}, ('type',))
    none = ensure('/keys', {'name': 'RCD 原生发布本地仓库', 'type': 'none'}, ('type',))
    repo = ensure('/repositories', {'name': 'RCD 原生发布已校验快照', 'git_url': repository,
                   'git_branch': 'snapshot', 'ssh_key_id': none['id']}, ('git_url','git_branch','ssh_key_id'))
    env = ensure('/environment', {'name': 'RCD 原生发布环境（无预存授权）', 'json': '{}',
                  'env': json.dumps({'ANSIBLE_HOST_KEY_CHECKING': 'True', 'ANSIBLE_PARAMIKO_HOST_KEY_CHECKING': 'True',
                                    'ANSIBLE_PARAMIKO_LOOK_FOR_KEYS': 'False'})}, ('json','env'))
    inventory = ensure('/inventory', {'name': 'RCD 三台原生发布库存', 'type': 'static-yaml',
                  'inventory': json.dumps({'all': {'children': {'rcd_nodes': {'hosts': inventory_hosts()}}}}),
                  'ssh_key_id': key['id'], 'become_key_id': sudo['id']}, ('type','inventory','ssh_key_id','become_key_id'))
    common = {'app':'ansible', 'type':'', 'inventory_id':inventory['id'], 'repository_id':repo['id'],
              'environment_id':env['id'], 'allow_override_args_in_task':False}
    stable = ('app','playbook','inventory_id','repository_id','environment_id')
    release = ensure('/templates', dict(common, name='RCD 软件发布（通过升级表单）',
                     playbook='tools/ansible/release-platform.yml', task_params={'skip_galaxy_install':True}), stable)
    control = ensure('/templates', dict(common, name='RCD 安全服务控制（通过设备详情）',
                     playbook='tools/ansible/control-platform.yml', task_params={'skip_galaxy_install':True}), stable) if service_control else None
    result = []
    for suffix in HOSTS:
        template = ensure('/templates', dict(common, name='RCD ' + suffix + ' 版本检查（原生）',
                          playbook='tools/ansible/inspect-platform.yml', task_params={'limit':['rcd'+suffix],'skip_galaxy_install':True}), stable)
        devices = api.request(prefix+'/rcd/devices?page_size=100')['items']
        device = next((d for d in devices if d['host_alias']=='rcd'+suffix), None)
        binding = {'name':'RCD '+suffix, 'address':'192.168.10.'+suffix, 'host_alias':'rcd'+suffix,
                   'inventory_id':inventory['id'], 'template_id':template['id'],
                   'release_template_id':release['id'], 'web_url':'http://192.168.10.'+suffix+':8087'}
        if device:
            if any(device.get(k)!=v for k,v in binding.items()):
                raise RuntimeError('existing device binding differs: '+suffix)
        else:
            device = api.request(prefix+'/rcd/devices', binding)
        if control:
            current = device.get('control_template_id', 0)
            if current not in (0, control['id']):
                raise RuntimeError('existing service control binding differs: ' + suffix)
            if not current:
                device = api.request(prefix+'/rcd/devices/'+str(device['id'])+'/control-binding',
                    {'revision': device['revision'], 'control_template_id': control['id']}, method='PUT',
                    headers={'If-Match': '"'+str(device['revision'])+'"'})
        if schedules:
            # Device creation already installs a native schedule. Repair only
            # a missing schedule; do not duplicate it under another display name.
            existing = [s for s in api.request(prefix+'/schedules') if s['template_id']==template['id'] and s.get('active')]
            if existing:
                if len(existing)!=1 or existing[0]['cron_format']!='* * * * *':
                    raise RuntimeError('review existing inspection schedules: '+suffix)
            else:
                ensure('/schedules', {'name':'RCD '+suffix+' 每分钟版本检查', 'template_id':template['id'],
                       'cron_format':'* * * * *', 'active':True, 'type':'', 'task_params':{'skip_galaxy_install':True}},
                       ('template_id','cron_format','active'))
        result.append({'device_id':device['id'], 'address':binding['address'], 'template_id':template['id']})
    return {'project_id':project, 'devices':result, 'release_template_id':release['id'],
            'scheduled':schedules, 'publications_started':False}


if __name__ == '__main__':
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--base-url', required=True)
    p.add_argument('--project-id', required=True, type=int)
    p.add_argument('--repository', required=True)
    p.add_argument('--login', default='admin')
    p.add_argument('--enable-schedules', action='store_true')
    p.add_argument('--enable-service-control', action='store_true', help='bind the controlled template after host preparation; does not run it')
    a = p.parse_args()
    api = Client(a.base_url, a.login, getpass.getpass('Semaphore password: '))
    result = register(api, a.project_id, a.repository, getpass.getpass('Node SSH/sudo password: '), a.enable_schedules, a.enable_service_control)
    print(json.dumps(result, ensure_ascii=False))
