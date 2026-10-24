#!/usr/bin/env python3
"""One-time, explicitly authorized wiring after compatible binaries are installed.

Owns the same host publication mutex throughout. Does not replace binaries or
business data. A failed/ambiguous stage is retained and never replayed. Ordinary
service controls must use Semaphore/Agent, not this installation entry point.
"""
import argparse
from pathlib import Path
import json
import os
import time

from component_deployment import ComponentDeployment
from deployment import durable_json, fingerprint, now, require_no_unfinished_release, target_lock
from release import Release, sha
from release_probe import read_json
from unified_install import prepare, dropin, owner, safe_write


class Bootstrap:
    def __init__(self, profiles, host, managers, fleet, operation, expected):
        self.profiles, self.host, self.managers, self.fleet = profiles, host, managers, fleet
        self.releases = {c: Release(read_json(profiles[c]), host, c) for c in ('lower', 'manager', 'agent')}
        self.d = ComponentDeployment(self.releases['agent'], operation)
        self.manager = ComponentDeployment(self.releases['manager'], operation)
        self.expected, self.locked = expected, False

    def assert_owned(self, shared):
        marker = read_json(self.d.host_marker)
        if (not self.locked or shared != self.d.release.entry.parent or
                marker.get('deployment_id') != self.d.id or marker.get('state') != 'running' or
                self.d.record.get('kind') != 'unified_install'):
            raise RuntimeError('installer does not own the original host transaction')

    def stop_service(self, component):
        release = self.releases[component]
        # These compatible units can be reloaded while running. The only early
        # change is disabling forced SIGKILL; the full binding is written stopped.
        account = owner(release)
        guard = dropin(release).with_name('79-rcd-graceful.conf')
        uid = account.pw_uid if release.p['control']['kind'] == 'systemd-user' else 0
        gid = account.pw_gid if uid else 0
        safe_write(guard, '[Service]\nSendSIGKILL=no\n', uid, gid)
        release.systemctl('daemon-reload')
        if release.systemctl('show', release.p['control']['unit'], '-p', 'SendSIGKILL', '--value').strip() != 'no':
            raise RuntimeError('graceful exit guard not active')
        release.systemctl('stop', '--no-block', release.p['control']['unit'])
        until = time.monotonic() + 45
        check = ComponentDeployment(release, self.d.id)
        while not check.confirmed_service_stopped():
            if time.monotonic() >= until:
                raise RuntimeError('exit unconfirmed; original transaction retained without killing')
            time.sleep(.25)
        return {'pid': 0, 'state': 'stopped'}

    def manager_enter(self):
        self.manager.record = self.d.record
        before = self.manager.manager_request()
        if before.get('active'):
            raise RuntimeError('manager already in maintenance')
        self.manager.manager_action('enter')
        until = time.monotonic() + 120
        while True:
            status = self.manager.manager_request()
            if status.get('deployment_id') != self.d.id or not status.get('active'):
                raise RuntimeError('manager maintenance identity changed')
            if status.get('drained'):
                return status
            if time.monotonic() >= until:
                raise RuntimeError('manager drain timed out; no service stop issued')
            time.sleep(.25)

    def execute(self, actor):
        if os.geteuid() != 0:
            raise ValueError('authorized root installer required')
        # Validate static bindings and the private fleet token before maintenance.
        prepare(self.profiles, self.host, self.managers, self.fleet)
        r, d = self.releases, self.d
        with target_lock(r['agent']):
            self.locked = True
            started = False
            try:
                for release in r.values():
                    require_no_unfinished_release(release)
                    release.preflight()
                if d.path.exists():
                    raise RuntimeError('original installation exists; inspect it, never replay')
                for component in ('agent', 'manager'):
                    if sha(r[component].entry) != self.expected or not r[component].pid():
                        raise RuntimeError('compatible manager and Agent must be running before first wiring')
                lower = d.lower.process_snapshot()
                if lower.get('pending_request_id') or lower['state'] not in ('running', 'stopped') or lower['state'] != lower['desired_state']:
                    raise RuntimeError('lower state is not confirmed')
                maintenance = d.lower.maintenance()
                if maintenance.get('job') and maintenance['job'].get('phase') != 'completed':
                    raise RuntimeError('existing lower maintenance must be reconciled first')
                d.directory.mkdir(parents=True, mode=0o700)
                d.record = {'schema_version': 1, 'kind': 'unified_install', 'deployment_id': d.id,
                    'host': self.host, 'component': 'agent', 'profile_sha256': fingerprint(r['agent'].p),
                    'binary_sha256': self.expected, 'target_version': 'unified-install', 'initiator': actor,
                    'was_running': True, 'lower_was_running': lower['state'] == 'running',
                    'created_at': now(), 'stages': []}
                d.mark('running')
                started = True
                self.manager.directory.mkdir(parents=True, mode=0o700)
                self.manager.record = d.record
                def backup():
                    result = {}
                    for component, release in r.items():
                        result[component] = str(release.backup(d.directory))
                        if component != 'lower':
                            unit = d.directory / (component + '-unit-before.txt')
                            safe_write(unit, release.systemctl('cat', release.p['control']['unit']), 0, 0)
                    return result
                d.step('backed_up', backup)
                d.step('manager_admission', self.manager_enter)
                d.step('lower_maintenance', d.enter)
                d.step('manager_stopped', lambda: self.stop_service('manager'))
                d.step('agent_stopped', lambda: self.stop_service('agent'))
                d.step('integration', lambda: prepare(self.profiles, self.host, self.managers, self.fleet, True, _bootstrap=self))
                d.step('agent_lower_resumed', lambda: d.restore_running(self.expected))
                def resume_manager():
                    r['manager'].systemctl('start', '--no-block', r['manager'].p['control']['unit'])
                    accepted = r['manager'].accept(self.expected)
                    self.manager.manager_action('complete')
                    return accepted
                d.step('manager_resumed', resume_manager)
                d.step('lower_maintenance_completed', d.complete)
                d.mark('succeeded')
                self.manager.save()
                return {'state': 'succeeded', 'operation_id': d.id, 'journal': str(d.path),
                        'lower_original_state': lower['state'], 'binary_sha256': self.expected}
            except BaseException:
                if started:
                    d.mark('needs_maintenance')
                raise
            finally:
                self.locked = False


if __name__ == '__main__':
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--profiles', type=Path, required=True)
    p.add_argument('--fleet', type=Path, required=True)
    p.add_argument('--host', required=True)
    p.add_argument('--manager-ips', required=True)
    p.add_argument('--operation-id', required=True)
    p.add_argument('--expected-sha256', required=True)
    p.add_argument('--initiator', required=True)
    p.add_argument('--confirm-paused', action='store_true')
    a = p.parse_args()
    if not a.confirm_paused:
        p.error('current control pause confirmation required')
    print(json.dumps(Bootstrap(read_json(a.profiles), a.host, a.manager_ips.split(','), read_json(a.fleet),
        a.operation_id, a.expected_sha256).execute(a.initiator), ensure_ascii=False))
