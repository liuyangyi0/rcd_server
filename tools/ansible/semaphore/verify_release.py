#!/usr/bin/env python3
"""Exercise registered upgrade templates in preview mode only. No restarts."""
import json
from pathlib import Path
import time
from connect_release import connection, RELEASE, write


def main():
    api = connection()
    registration = json.loads(Path('/etc/semaphore/release-bootstrap.json').read_text())
    prefix = '/project/' + str(registration['project_id'])
    results = []
    for template in registration['templates']:
        if template['workflow'] != 'deploy':
            continue
        actual = api(prefix + '/templates/' + str(template['id']))
        if actual['playbook'] != 'tools/ansible/semaphore-release.yml':
            raise RuntimeError('unexpected template; refused')
        for component in ['lower', 'agent'] + (['manager'] if not template['host'].endswith('.30') else []):
            task = api(prefix + '/tasks', {'template_id': template['id'], 'message': '接入验证：仅检查，不升级、不重启',
                'environment': json.dumps({'component': component, 'release_id': RELEASE,
                    'execution': 'preview', 'command_pause': 'not_confirmed'}), 'params': {'dry_run': False}})
            path = prefix + '/tasks/' + str(task['id'])
            deadline = time.monotonic() + 120
            while time.monotonic() < deadline:
                state = api(path)
                if state['status'] in ('success', 'error', 'stopped'):
                    break
                time.sleep(1)
            output = api(path + '/output')
            result = {'host': template['host'], 'component': component, 'task_id': task['id'], 'status': state['status']}
            print(json.dumps(result), flush=True)
            results.append(dict(result, output=output))
            write('/var/lib/semaphore/release-preview-verification.json', results, 0o600)
            if state['status'] != 'success':
                print(json.dumps(output, ensure_ascii=False))
                raise RuntimeError('preview failed; no live execution attempted')


if __name__ == '__main__':
    main()
