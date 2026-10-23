"""Restore the original Semaphore binary and organize existing native templates.

One-time UI migration on controller 46; no task submissions or RCD service changes.
Table column preferences are browser-local in upstream, not server settings.
"""
import argparse
import copy
import hashlib
import json
import os
from pathlib import Path
import shutil
import sqlite3
import subprocess
import sys
import time
import urllib.request

sys.path.insert(0, '/opt/semaphore/playbooks/tools/ansible/semaphore')
from connect_release import connection


def classify(template):
    if template['playbook'] == 'tools/ansible/inspect.yml':
        return '状态检查'
    if template['playbook'] == 'tools/ansible/semaphore-release.yml':
        args = json.loads(template['arguments'])
        workflow = json.loads(args[args.index('--extra-vars') + 1])['rcd_workflow']
        return {'deploy': '软件升级', 'rollback': '版本回退'}[workflow]
    raise ValueError('unexpected template: ' + template['name'])


def schema(path):
    with sqlite3.connect('file:' + str(path) + '?mode=ro', uri=True) as db:
        assert db.execute('PRAGMA quick_check').fetchone() == ('ok',)
        return db.execute('SELECT type,name,sql FROM sqlite_master ORDER BY type,name').fetchall()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--apply', action='store_true')
    args = parser.parse_args()
    assert os.geteuid() == 0
    assert '192.168.10.46' in subprocess.check_output(['hostname', '-I'], text=True).split()
    api = connection()
    prefix = '/project/1'
    templates = [api(prefix + '/templates/' + str(t['id'])) for t in api(prefix + '/templates')]
    views = api(prefix + '/views')
    tasks = api(prefix + '/tasks?count=200')
    assert not any(t['status'] in ('waiting', 'starting', 'running', 'stopping') for t in tasks)
    assert len(templates) == 9, 'review unexpected templates before migration'
    groups = {name: [t['id'] for t in templates if classify(t) == name]
              for name in ('状态检查', '软件升级', '版本回退')}
    assert all(len(ids) == 3 for ids in groups.values())
    print(json.dumps({'groups': groups, 'views': views, 'tasks_before': len(tasks)}, ensure_ascii=False))
    record_path = Path('/opt/semaphore/operator-ui-record.json')
    record = json.loads(record_path.read_text())
    previous = Path(record['backup']) / 'semaphore'
    assert str(previous.resolve()).startswith('/var/lib/semaphore/backups/operator-ui-ui-')
    assert subprocess.check_output([str(previous), 'version'], text=True).strip() == record['old_version']
    current = Path('/opt/semaphore/bin/semaphore')
    digest = lambda p: hashlib.sha256(p.read_bytes()).hexdigest()
    assert digest(current) == record['new_binary_sha256'], 'live binary changed; review before restore'
    if not args.apply:
        return
    backup = Path('/var/lib/semaphore/backups') / ('native-layout-' + str(time.time_ns()))
    backup.mkdir(mode=0o700)
    cfg = Path('/etc/semaphore/config.json')
    settings = json.loads(cfg.read_text())
    db_path = Path(settings['sqlite']['host'])
    assert str(db_path) == '/var/lib/semaphore/semaphore.sqlite'
    shutil.copy2(current, backup / 'semaphore')
    shutil.copy2(cfg, backup / 'config.json')
    shutil.copy2(record_path, backup / record_path.name)
    (backup / 'templates-and-views.json').write_text(json.dumps({'templates': templates, 'views': views}, ensure_ascii=False))
    before = schema(db_path)
    subprocess.run(['systemctl', 'stop', 'semaphore.service'], check=True)
    try:
        with sqlite3.connect('file:' + str(db_path) + '?mode=ro', uri=True) as src, sqlite3.connect(backup / 'semaphore.sqlite') as dst:
            src.backup(dst)
        staged = current.with_name('semaphore.native-new')
        shutil.copy2(previous, staged)
        staged.chmod(0o755)
        os.replace(staged, current)
        subprocess.run(['systemctl', 'start', 'semaphore.service'], check=True)
        for attempt in range(30):
            try:
                with urllib.request.urlopen(settings['web_host'] + '/api/ping', timeout=2) as response:
                    assert response.read() == b'pong'
                break
            except Exception:
                if attempt == 29:
                    raise
                time.sleep(1)
        assert schema(db_path) == before
        assert digest(cfg) == digest(backup / 'config.json')
    except Exception:
        subprocess.run(['systemctl', 'stop', 'semaphore.service'], check=True)
        shutil.copy2(backup / 'semaphore', current)
        subprocess.run(['systemctl', 'start', 'semaphore.service'], check=True)
        raise
    api = connection()
    saved_views = {}
    for position, title in enumerate(groups):
        existing = next((v for v in views if v['title'] == title), None)
        body = dict(existing or {}, project_id=1, title=title, position=position,
                    type='', sort_column='name', sort_reverse=False, hidden=False)
        if existing:
            api(prefix + '/views/' + str(existing['id']), body, 'PUT')
            saved_views[title] = existing['id']
        else:
            saved_views[title] = api(prefix + '/views', body)['id']
    for template in templates:
        body = copy.deepcopy(template)
        body.pop('last_task', None)
        body.pop('last_task_id', None)
        body['view_id'] = saved_views[classify(template)]
        for variable in body.get('survey_vars') or []:
            if variable['name'] == 'command_pause':
                for value in variable.get('values') or []:
                    if value['value'] == 'confirmed':
                        value['name'] = '已暂停 30、46、58 管理端及直连客户端的命令'
        api(prefix + '/templates/' + str(template['id']), body, 'PUT')
    # Preserve ALL/custom existing views, after the three daily tabs.
    positions = {str(v['id']): i + 3 for i, v in enumerate(views) if v['id'] not in saved_views.values()}
    positions.update({str(v): i for i, v in enumerate(saved_views.values())})
    api(prefix + '/views/positions', positions, 'POST')
    after_tasks = api(prefix + '/tasks?count=200')
    assert [t['id'] for t in tasks] == [t['id'] for t in after_tasks], 'task history changed'
    for title, view_id in saved_views.items():
        assert sorted(t['id'] for t in api(prefix + '/views/' + str(view_id) + '/templates')) == sorted(groups[title])
    result = {'backup': str(backup), 'official_version': record['old_version'],
              'binary_sha256': digest(current), 'views': saved_views, 'tasks_preserved': True,
              'opc_services_changed': False, 'column_preferences': 'browser-local'}
    Path('/opt/semaphore/native-layout-record.json').write_text(json.dumps(result, ensure_ascii=False, indent=2))
    print(json.dumps(result, ensure_ascii=False))


if __name__ == '__main__':
    main()
