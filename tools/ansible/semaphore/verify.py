#!/usr/bin/env python3
"""Verify installed platform by executing ONLY its registered read-only templates."""
import http.cookiejar
import json
import os
from pathlib import Path
import time
import urllib.error
import urllib.request


def main():
    config = json.loads(Path('/etc/semaphore/config.json').read_text())
    registration = json.loads(Path('/etc/semaphore/project-bootstrap.json').read_text())
    base = config['web_host'] + '/api'
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}),
        urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))

    def api(path, body=None):
        with opener.open(urllib.request.Request(base + path,
                data=json.dumps(body).encode() if body is not None else None,
                headers={'Content-Type': 'application/json'}), timeout=20) as response:
            data = response.read()
            return json.loads(data) if data else None

    api('/auth/login', {'auth': 'admin', 'password': os.environ.get('SEMAPHORE_ADMIN_PASSWORD') or Path('/etc/semaphore/initial-admin-password').read_text().strip()})
    project = '/project/' + str(registration['project_id'])
    results = []
    for template in registration['templates']:
        actual = api(project + '/templates/' + str(template['id']))
        if actual['playbook'] != 'tools/ansible/inspect.yml' or actual['app'] != 'ansible':
            raise RuntimeError('only read-only RCD inspection may be tested')
        task = api(project + '/tasks', {'template_id': template['id'], 'debug': False, 'dry_run': False,
                                        'message': 'Semaphore installation: read-only software inspection'})
        path = project + '/tasks/' + str(task['id'])
        deadline = time.monotonic() + 90
        while time.monotonic() < deadline:
            status = api(path)
            if status['status'] in ('success', 'error', 'stopped'):
                break
            time.sleep(1)
        output = api(path + '/output')
        results.append({'host': template['host'], 'task_id': task['id'], 'status': status['status'], 'output': output})
        print(json.dumps({'host': template['host'], 'task_id': task['id'], 'status': status['status']}), flush=True)
        if status['status'] != 'success':
            # Only read-only command output, never access keys or cookie headers.
            print(json.dumps(output, ensure_ascii=False))
            raise RuntimeError('inspection failed; no deployment was attempted')
    path = Path('/var/lib/semaphore/installation-verification.json')
    path.write_text(json.dumps({'url': config['web_host'], 'results': results}, ensure_ascii=False, indent=2))
    path.chmod(0o600)


if __name__ == '__main__':
    main()
