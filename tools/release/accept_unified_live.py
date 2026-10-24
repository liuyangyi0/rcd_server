"""Explicit, one-action-at-a-time live acceptance helpers (remote 46 only).

No actions run on import. Callers supply authenticated native API clients and
the current control-pause confirmation. Evidence precedes every write. A lost
reply is never retried here: query the original request in the retained receipt.
There are no serial commands, configuration writes, or fallback SSH controls.
"""
import json
import os
from pathlib import Path
import time
import urllib.error
import urllib.request
import uuid


SOURCES = {'30': 'rcd-30', '58': 'rcd-58', '46': 'rcd-46'}
# Agent instance identities do not change when manager source IDs are migrated.
# Persistent physical Agent identities, intentionally distinct from OPC source IDs.
AGENT_INSTANCES = {'30': 'rcd93-30', '58': 'rcd92-58', '46': 'real-lower'}


def evidence(path, value, exclusive=False):
    path = Path(path)
    with path.open('x' if exclusive else 'w', encoding='utf-8') as output:
        os.fchmod(output.fileno(), 0o600)
        json.dump(value, output, ensure_ascii=False, indent=2)
        output.flush()
        os.fsync(output.fileno())


def agents():
    result = {}
    for suffix, instance in AGENT_INSTANCES.items():
        url = 'http://192.168.10.' + suffix + ':8090/api/v1/process'
        request = urllib.request.Request(url, headers={'X-RCD-Instance-ID': instance})
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        with opener.open(request, timeout=8) as response:
            status = json.load(response)
        assert status['instance_id'] == instance and status['lifecycle_supported']
        result[suffix] = status
    return result


def stable_agents(before, after, changed_lower=None):
    for suffix in SOURCES:
        assert before[suffix]['agent_identity']['boot_id'] == after[suffix]['agent_identity']['boot_id'], 'unrelated Agent restarted'
        if suffix != changed_lower:
            assert before[suffix].get('pid') == after[suffix].get('pid'), 'unrelated lower PID changed'
            assert before[suffix]['desired_state'] == after[suffix]['desired_state']


def lower_action(client, suffix, action, output, *, confirmed=False):
    if not confirmed or suffix not in SOURCES or action not in ('start', 'stop', 'restart'):
        raise ValueError('current pause confirmation and fixed target/action required')
    output = Path(output)
    before = agents()
    base = '/api/v1/sources/' + SOURCES[suffix] + '/process'
    status = client.request(base + '/status')
    assert status['lifecycle_supported'] and not status.get('pending_request_id')
    assert not status.get('maintenance_deployment_id')
    assert status['state'] == ('stopped' if action == 'start' else 'running')
    request_id = 'unified-m' + client.suffix + '-l' + suffix + '-' + action + '-' + uuid.uuid4().hex[:16]
    body = {'request_id': request_id, 'source_revision': status['source_revision'],
            'binding_revision': status['binding_revision'], 'action': action}
    receipt = {'manager': client.suffix, 'target': suffix, 'body': body,
               'revision': status['revision'], 'before': before}
    evidence(output / (request_id + '-request.json'), receipt, True)
    result = client.request(base + '/actions', body, 'POST', {'If-Match': '"' + status['revision'] + '"'})
    end = time.monotonic() + 145
    while result['state'] in ('accepted', 'running') and time.monotonic() < end:
        time.sleep(1)
        result = client.request(base + '/operations/' + request_id)
        evidence(output / (request_id + '-result.json'), result)
    evidence(output / (request_id + '-result.json'), result)
    assert result['state'] == 'succeeded', 'inspect original operation; never repeat: ' + request_id
    assert not result.get('forced')
    after = agents()
    stable_agents(before, after, suffix)
    value = after[suffix]
    assert value['state'] == ('stopped' if action == 'stop' else 'running')
    assert value['desired_state'] == value['state']
    assert not value.get('maintenance_deployment_id')
    assert bool(value.get('manual_stopped')) == (action == 'stop')
    if action == 'restart':
        assert value['pid'] != before[suffix]['pid']
    evidence(output / (request_id + '-acceptance.json'), {'passed': True, 'after': after})
    print(json.dumps({'request_id': request_id, 'manager': client.suffix,
                      'target': suffix, 'action': action, 'state': result['state']}, ensure_ascii=False), flush=True)
    return result


def platform_control(api, suffix, action, component, output, *, confirmed=False):
    if not confirmed or suffix not in SOURCES or not (
            component == 'manager' and action in ('start', 'stop', 'restart') or
            component == 'agent' and action == 'recover'):
        raise ValueError('current pause confirmation and fixed target/action required')
    devices = api.request('/project/1/rcd/devices?page_size=100')['items']
    device = next(d for d in devices if d['host_alias'] == 'rcd' + suffix)
    body = {'kind': 'service_control', 'component': component, 'action': action,
            'commands_paused': True, 'targets': [{'device_id': device['id'], 'revision': device['revision']}]}
    key = 'unified-control-' + suffix + '-' + action + '-' + uuid.uuid4().hex
    output = Path(output)
    evidence(output / (key + '-request.json'), {'body': body, 'key': key}, True)
    result = api.request('/project/1/rcd/operations', body, 'POST',
                         {'If-Match': '"' + str(device['revision']) + '"', 'Idempotency-Key': key})
    evidence(output / (key + '-accepted.json'), result, True)
    print(json.dumps({k: result.get(k) for k in ('id', 'state', 'task_id')}), flush=True)
    return result


def inspect_device(api, suffix, output, timeout=50):
    device = next(d for d in api.request('/project/1/rcd/devices?page_size=100')['items']
                  if d['host_alias'] == 'rcd' + suffix)
    base = '/project/1/rcd/devices/' + device['id']
    task = api.request(base + '/inspections', {'revision': device['revision']}, 'POST',
                       {'If-Match': '"' + str(device['revision']) + '"'})
    end = time.monotonic() + timeout
    while True:
        device = api.request(base)
        if device['last_task_id'] >= task['task_id']:
            evidence(Path(output) / ('inspection-' + str(task['task_id']) + '.json'), device)
            assert device['state'] == 'checked', 'inspection did not succeed'
            return device
        if time.monotonic() >= end:
            raise TimeoutError('original read-only inspection still pending: ' + str(task['task_id']))
        time.sleep(2)


def manager_pids(password):
    """Read actual systemd PIDs via the existing, verified SSH host keys."""
    import paramiko
    result = {}
    for suffix in SOURCES:
        if suffix == '46':
            # Acceptance runs on 46. Do not establish a new SSH host-key trust
            # relationship merely to inspect our own user service.
            import subprocess
            network = subprocess.run(['ip', '-j', 'address'], check=True,
                                     capture_output=True, text=True)
            assert any(address.get('local') == '192.168.10.46'
                       for link in json.loads(network.stdout)
                       for address in link.get('addr_info', [])), 'run on remote 46 only'
            current = subprocess.run(['systemctl', '--user', 'show', 'rcd-manager.service',
                                      '-p', 'MainPID', '--value'], check=True,
                                     env={**os.environ, 'XDG_RUNTIME_DIR': '/run/user/' + str(os.getuid())},
                                     capture_output=True, text=True)
            result[suffix] = int(current.stdout.strip())
            continue
        client = paramiko.SSHClient()
        client.load_system_host_keys()
        try:
            client.connect('192.168.10.' + suffix,
                           username='RCD93' if suffix == '30' else 'lenovo',
                           password=password, look_for_keys=False, allow_agent=False,
                           timeout=10)
            _, stdout, stderr = client.exec_command(
                'XDG_RUNTIME_DIR=/run/user/$(id -u) systemctl --user show '
                'rcd-manager.service -p MainPID --value', timeout=10)
            value = stdout.read().decode().strip()
            assert stdout.channel.recv_exit_status() == 0, stderr.read().decode()
            result[suffix] = int(value)
        finally:
            client.close()
    return result


if __name__ == '__main__':
    import sys
    if sys.argv[1:] != ['--manager-pids']:
        raise SystemExit('Only --manager-pids is available on CLI (read-only; password on stdin).')
    print(json.dumps(manager_pids(sys.stdin.read().rstrip('\n'))))
