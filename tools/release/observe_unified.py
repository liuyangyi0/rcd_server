"""Bounded read-only post-release observation; no restart/config/control calls.

Run independently on an explicitly selected host. JSONL retains HTTP health
details as well as status codes: HTTP 200 does not imply no audit warnings.
The process exits at its deadline; it is not a new business daemon.
"""
import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import socket
import re
import subprocess
import time
import urllib.request

from inspect_runtime import inspect


def http_json(url, headers=None):
    request = urllib.request.Request(url, headers=headers or {})
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open(request, timeout=8) as response:
        raw = response.read(2 * 1024 * 1024 + 1)
        if len(raw) > 2 * 1024 * 1024:
            raise ValueError('read-only response exceeds limit')
        return response.status, json.loads(raw)


def snapshot(host):
    instance = {'30': 'rcd93-30', '58': 'rcd92-58', '46': 'real-lower'}[host]
    login = 'RCD93' if host == '30' else 'lenovo'
    roots = ['/var/lib/rcd-release-platform/artifacts/' + c for c in ('lower', 'manager', 'agent')]
    row = {'at': datetime.now(timezone.utc).isoformat(), 'host': host, 'issues': [], 'health_warnings': {}}
    identities = []
    try:
        row['probes'] = [
            inspect('manager', 'rcd-manager.service', 'user', instance, login, roots),
            inspect('lower-stack', 'rcd-process-agent@real-lower.service' if host == '46' else 'rcd-lower.service',
                    'user' if host == '46' else 'system', instance, login, roots)]
        for probe in row['probes']:
            service = probe['service']
            identities.append([probe['component'], service['MainPID'], service['NRestarts']])
            if service['ActiveState'] != 'active' or probe.get('inspection_error'):
                row['issues'].append('service_not_confirmed:' + probe['component'])
            for item in probe['processes']:
                identities.append([item['component'], item.get('pid'), item.get('running_sha256')])
                if not item.get('running') or not item.get('running_build') or item.get('restart_required_for_entry'):
                    row['issues'].append('process_not_confirmed:' + item['component'])
        for port in (4840, 4841):
            with socket.create_connection(('127.0.0.1', port), timeout=4):
                pass
        row['manager_http_status'], row['manager_health'] = http_json('http://127.0.0.1:8087/healthz')
        checks = row['manager_health'].get('checks', {})
        row['health_warnings'] = {k: v for k, v in checks.items() if v not in ('ok', 'disabled')}
        _, status = http_json('http://127.0.0.1:8090/api/v1/process', {'X-RCD-Instance-ID': instance})
        row['agent'] = status
        if status['state'] != 'running' or status.get('manual_stopped') or status.get('pending_request_id') or status.get('maintenance_deployment_id'):
            row['issues'].append('lower_process_or_maintenance_not_idle')
        if host == '46':
            text = subprocess.check_output(['systemctl', 'show', 'semaphore.service', '-p', 'MainPID',
                                            '-p', 'ActiveState', '-p', 'NRestarts'], text=True, timeout=8)
            platform = dict(line.split('=', 1) for line in text.splitlines() if '=' in line)
            pid = int(platform['MainPID'])
            if pid <= 0 or platform['ActiveState'] != 'active':
                raise RuntimeError('Semaphore not active')
            digest = hashlib.sha256()
            with open('/proc/' + str(pid) + '/exe', 'rb') as stream:
                for block in iter(lambda: stream.read(1024 * 1024), b''):
                    digest.update(block)
            platform['sha256'] = digest.hexdigest()
            row['platform'] = platform
            identities.append(['platform', str(pid), platform['NRestarts'], platform['sha256']])
            # A native project guest token is read privately; only the received
            # non-secret version snapshot is retained, never request headers.
            token = Path('/var/lib/rcd/manager/main/semaphore-readonly.token').read_text().strip()
            # The platform intentionally binds only the fixed management IP,
            # not loopback. Do not change its listener to accommodate a probe.
            _, row['fleet'] = http_json('http://192.168.10.46:3000/api/project/1/rcd/fleet', {'Authorization': 'Bearer ' + token})
            for device in row['fleet']['machines']:
                # Go emits nanoseconds; system Python 3.8 accepts microseconds.
                raw_stamp = re.sub(r'(\.\d{6})\d+', r'\1', device['last_success_at'])
                stamp = datetime.fromisoformat(raw_stamp.replace('Z', '+00:00'))
                if (datetime.now(timezone.utc) - stamp).total_seconds() > 180:
                    row['issues'].append('platform_snapshot_stale:' + device['id'])
    except Exception as error:
        # Only bounded, read-only probes use these messages; no credentials or
        # configuration body is included in an exception by this module.
        row['issues'].append(type(error).__name__ + ':' + str(error))
    row['identities'] = identities
    return row


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--host', choices=['30', '58', '46'], required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--seconds', type=int, default=86400)
    args = parser.parse_args()
    if not 1 <= args.seconds <= 86400:
        parser.error('duration must be 1..86400 seconds')
    start, baseline, count = time.monotonic(), None, 0
    with args.output.open('x', encoding='utf-8') as output:
        os.fchmod(output.fileno(), 0o600)
        while True:
            row = snapshot(args.host)
            if baseline is None and not row['issues']:
                baseline = row['identities']
            elif baseline is not None and row['identities'] != baseline:
                row['issues'].append('process_identity_or_restart_count_changed')
            row['elapsed_seconds'] = round(time.monotonic() - start)
            output.write(json.dumps(row, ensure_ascii=False) + '\n')
            output.flush()
            count += 1
            due = start + count * 60
            if due - start > args.seconds:
                break
            time.sleep(max(0, due - time.monotonic()))


if __name__ == '__main__':
    main()
