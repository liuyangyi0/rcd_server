#!/usr/bin/env python3
"""Native Semaphore service control. Fixed profiles, no package and no replay."""
import argparse
import json
import os
from pathlib import Path
import re
import time

from component_deployment import ComponentDeployment
from deployment import durable_json, fingerprint, now, require_no_unfinished_release, target_lock, sync_directory
from release import Release, sha
from release_probe import read_json, public_error, ReleaseProblem


class ServiceControl:
    def __init__(self, release, operation, action, expected, actor, report_binding):
        if not re.fullmatch('[a-f0-9]{64}', operation) or not re.fullmatch('[a-f0-9]{64}', expected):
            raise ValueError('pinned operation and image digest required')
        component = release.p['component']
        if not (((component == 'manager' or release.application) and action in ('start', 'stop', 'restart')) or
                (component == 'agent' and action == 'recover')):
            raise ValueError('service target/action is not supported')
        self.r, self.action, self.expected, self.actor = release, action, expected, actor
        self.d = ComponentDeployment(release, operation)
        self.hold = release.runtime / 'manual-stop.json'
        self.report_binding = report_binding
        self.acceptance = {}

    def mark(self, state):
        self.d.record['state'] = state
        self.d.save()
        durable_json(self.d.host_marker, {'deployment_id': self.d.id, 'component': self.r.p['component'],
                                         'kind': 'service_control', 'state': state, 'record': str(self.d.path)})

    def manager_change(self, action, owner):
        before = self.d.manager_request()
        if before.get('active') and before.get('deployment_id') != owner:
            raise RuntimeError('manager belongs to another maintenance operation')
        identity = self.d.record.setdefault('manager_instance_id', before['instance_id'])
        if before['instance_id'] != identity:
            raise RuntimeError('manager instance changed')
        self.d.save()
        result = self.d.manager_request({'action': action, 'instance_id': identity,
                                        'deployment_id': owner, 'expected_revision': before['revision']})
        if result.get('deployment_id') != owner or result.get('active') != (action == 'enter'):
            raise RuntimeError('manager maintenance change unconfirmed')
        return result

    def require_manager_guard(self):
        unit = self.r.p['control']['unit']
        conditions = []
        for line in self.r.systemctl('cat', unit).splitlines():
            if line.strip().startswith('ConditionPathExists='):
                value = line.strip().split('=', 1)[1]
                if not value: conditions = []
                else:
                    # Do not shell-unquote: systemd's condition parser doesn't
                    # accept the same quoting rules as ExecStart.
                    conditions.append(value)
        kill = self.r.systemctl('show', unit, '-p', 'SendSIGKILL', '--value').strip()
        if '!' + str(self.hold) not in conditions or kill != 'no':
            raise ReleaseProblem('service_guard_missing', '尚未安装管理端人为停止保护和禁止强杀设置，请先完成接入')

    def manager_stop(self):
        pid = self.r.pid()
        owner = ''
        if pid:
            if sha('/proc/' + str(pid) + '/exe') != self.expected:
                raise RuntimeError('running manager image changed')
            owner = self.d.id
            self.d.step('maintenance_enter', lambda: self.manager_change('enter', owner))
            end = time.monotonic() + 120
            while True:
                status = self.d.manager_request()
                if not status.get('active') or status.get('deployment_id') != owner:
                    raise RuntimeError('manager maintenance ownership changed')
                if status.get('drained'):
                    break
                if time.monotonic() >= end:
                    raise ReleaseProblem('drain_timeout', '管理端在途事务排空超时；未停止服务，保留维护供核对')
                time.sleep(.25)
        elif self.hold.exists():
            owner = read_json(self.hold).get('fence_id', '')
        else:
            saved = self.r.runtime / 'release-maintenance.json'
            if saved.exists() and read_json(saved).get('active'):
                raise RuntimeError('stopped manager has unrelated unfinished maintenance')
        def hold_and_stop():
            durable_json(self.hold, {'schema_version': 1, 'desired_state': 'stopped', 'fence_id': owner,
                                    'operation_id': self.d.id, 'manager_instance_id': self.d.record.get('manager_instance_id', '')})
            if pid:
                self.r.systemctl('stop', '--no-block', self.r.p['control']['unit'])
                end = time.monotonic() + 40
                while not self.d.confirmed_service_stopped():
                    if time.monotonic() >= end:
                        raise ReleaseProblem('exit_unconfirmed', '管理端退出尚未确认；未强杀，请核对原作业')
                    time.sleep(.25)
            if self.r.pid():
                raise RuntimeError('manager still running')
            return {'state': 'stopped', 'pid': 0, 'binary_sha256': self.expected}
        self.acceptance = self.d.step('service_stop', hold_and_stop)

    def manager_start(self):
        hold = read_json(self.hold) if self.hold.exists() else {}
        fence_id = hold.get('fence_id', '')
        saved_path = self.r.runtime / 'release-maintenance.json'
        saved = read_json(saved_path) if saved_path.exists() else {}
        if saved.get('active') and (not fence_id or saved.get('deployment_id') != fence_id):
            raise RuntimeError('manager retained maintenance is not the confirmed manual stop')
        def start():
            # Durable intent precedes removing the boot inhibit. If replies are
            # lost, the original task remains unknown; no automatic second start.
            if self.hold.exists():
                self.hold.unlink()
                sync_directory(self.hold.parent)
            self.r.systemctl('start', '--no-block', self.r.p['control']['unit'])
            result = self.r.accept(self.expected)
            status = self.d.manager_request()
            if hold.get('manager_instance_id') and status['instance_id'] != hold['manager_instance_id']:
                raise RuntimeError('manager identity changed after manual stop')
            if fence_id:
                self.manager_change('complete', fence_id)
            elif status.get('active'):
                raise RuntimeError('manager unexpectedly entered maintenance')
            result['state'] = 'running'
            return result
        self.acceptance = self.d.step('service_start', start)

    def recover_agent(self):
        # Start only a conclusively stopped Agent. Do not rewrite desired state,
        # maintenance IDs, or child records in an attempt to "repair" it.
        if not self.d.confirmed_service_stopped():
            raise RuntimeError('Agent is not conclusively stopped')
        self.d.ensure_no_process_images({str(self.r.entry), str(self.d.lower.release.entry)})
        state = read_json(self.r.runtime / 'process-state.json', 8 << 20)
        if state.get('instance_id') != self.d.lower.release.p['control']['instance_id']:
            raise RuntimeError('Agent state identity mismatch')
        # Confirm nobody owns the actual Agent lock before sending systemd the
        # one start request. Release it immediately before systemd must acquire
        # it; the shared host lock remains held for the entire operation.
        import fcntl
        import stat
        fd = os.open(self.r.runtime / 'agent.lock', os.O_RDWR | os.O_NOFOLLOW | os.O_NONBLOCK)
        try:
            info = os.fstat(fd)
            if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
                raise RuntimeError('Agent lock identity invalid')
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            if not self.d.confirmed_service_stopped():
                raise RuntimeError('Agent service changed before recovery')
        finally:
            os.close(fd)
        def start():
            self.r.systemctl('start', '--no-block', self.r.p['control']['unit'])
            result = self.r.accept(self.expected)
            snapshot = self.d.lower.process_snapshot()
            if state.get('desired_state') == 'stopped' and (snapshot.get('pid') or snapshot.get('desired_state') != 'stopped'):
                raise RuntimeError('Agent did not preserve manually stopped child')
            pending = state.get('lifecycle') or {}
            if pending and pending.get('state') not in ('succeeded', 'failed'):
                job = snapshot.get('lifecycle') or {}
                if job.get('request_id') != pending.get('request_id') or job.get('state') != 'unknown' or snapshot.get('pid'):
                    raise RuntimeError('Agent recovery changed or replayed unfinished lower operation')
                result['lower_operation_pending'] = True
            result['state'] = 'running'
            return result
        self.acceptance = self.d.step('agent_recovery', start)

    def application_stop(self):
        self.r.require_application_service()
        pid = self.r.pid()
        if not pid or sha('/proc/' + str(pid) + '/exe') != self.expected:
            raise RuntimeError('registered application running identity changed')
        def stop():
            durable_json(self.hold, {'schema_version': 1, 'desired_state': 'stopped', 'operation_id': self.d.id})
            self.r.systemctl('stop', '--no-block', self.r.p['control']['unit'])
            end = time.monotonic() + 40
            while not self.d.confirmed_service_stopped():
                if time.monotonic() >= end:
                    raise ReleaseProblem('exit_unconfirmed', '软件正常退出未确认；未强杀，保留原作业且不会重复启停')
                time.sleep(.25)
            self.d.stopped()
            return {'state': 'stopped', 'pid': 0, 'binary_sha256': self.expected, 'normal_process_stop_only': True}
        self.acceptance = self.d.step('application_stop', stop)

    def application_start(self):
        self.r.require_application_service()
        self.d.stopped()
        def start():
            if self.hold.exists():
                hold = read_json(self.hold)
                if hold.get('desired_state') != 'stopped':
                    raise RuntimeError('registered application manual-stop identity invalid')
                self.hold.unlink();sync_directory(self.hold.parent)
            # Initial install intentionally leaves the service disabled. Only an
            # explicit confirmed start opts it into normal boot startup.
            self.r.systemctl('enable', self.r.p['control']['unit'])
            self.r.systemctl('start', '--no-block', self.r.p['control']['unit'])
            result = self.r.accept(self.expected)
            result.update(state='running', normal_process_stop_only=True)
            return result
        self.acceptance = self.d.step('application_start', start)

    def execute(self):
        self.r.local_host()
        with target_lock(self.r):
            require_no_unfinished_release(self.r, allow_agent_recovery=self.r.p['component'] == 'agent')
            if self.d.path.exists():
                raise RuntimeError('operation already recorded; inspect original evidence, never replay')
            if not self.r.application:
                self.r.preflight()
            if sha(self.r.entry) != self.expected:
                raise RuntimeError('pinned disk image changed')
            if self.r.p['component'] == 'manager':
                self.require_manager_guard()
            self.d.directory.mkdir(parents=True, mode=0o700)
            self.d.record = {'schema_version': 1, 'kind': 'service_control', 'deployment_id': self.d.id,
                             'component': self.r.p['component'], 'host': self.r.p['host'],
                             'profile_sha256': fingerprint(self.r.p), 'initiator': self.actor,
                             'action': self.action, 'created_at': now(), 'stages': [],
                             'report_binding': self.report_binding, 'binary_sha256': self.expected}
            self.mark('running')
            try:
                if self.r.application:
                    # First installation never starts a service. Load the newly
                    # installed fixed unit only after durable native intent, then
                    # inspect the effective unit before any process action.
                    self.d.step('application_unit_reload', lambda: self.r.systemctl('daemon-reload'))
                    self.r.preflight()
                    pid=self.r.pid()
                    if (self.action=='start' and pid) or (self.action in ('stop','restart') and not pid):
                        raise RuntimeError('registered application action no longer matches process state')
                if self.r.p['component'] == 'agent':
                    self.recover_agent()
                elif self.r.application:
                    if self.action in ('stop','restart'): self.application_stop()
                    if self.action in ('start','restart'): self.application_start()
                else:
                    if self.action in ('stop', 'restart'):
                        self.manager_stop()
                    if self.action in ('start', 'restart'):
                        self.manager_start()
                self.d.record['acceptance'] = self.acceptance
                self.mark('succeeded')
            except Exception:
                self.mark('needs_maintenance')
                raise
        return self.acceptance


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--profile', type=Path, required=True)
    for name in ('host', 'component', 'operation-id', 'device-id', 'host-alias', 'action', 'expected-sha256', 'initiator'):
        p.add_argument('--' + name, required=True)
    p.add_argument('--task-id', type=int, required=True)
    p.add_argument('--commands-paused', action='store_true')
    p.add_argument('--inspect', action='store_true')
    p.add_argument('--project-id', type=int)
    p.add_argument('--definition-sha256')
    args = p.parse_args()
    r = Release(read_json(args.profile), args.host, args.component)
    if r.application and (r.p['application']['project_id'] != args.project_id or r.p['application']['definition_sha256'] != args.definition_sha256):
        raise ValueError('registered project/application identity does not match installed service')
    binding = {'schema_version': 1, 'operation_id': args.operation_id, 'task_id': args.task_id,
               'device_id': args.device_id, 'host_alias': args.host_alias}
    op = ServiceControl(r, args.operation_id, args.action, args.expected_sha256, args.initiator, binding)
    output = r.root / 'platform-reports' / (args.operation_id + '.json')
    if args.inspect:
        print(json.dumps(read_json(output), ensure_ascii=False))
        return
    if not args.commands_paused or args.task_id < 1:
        p.error('current pause confirmation and native task required')
    # Never overwrite an earlier report when the same execution is requested.
    if output.exists() or op.d.path.exists():
        raise RuntimeError('original operation exists; use read-only inspection')
    report = dict(binding, kind='service_control', state='failed_before_execution', binary_sha256=args.expected_sha256,
                  stages=[], acceptance={}, error='')
    try:
        report['acceptance'] = op.execute()
        report['state'] = 'succeeded'
    except Exception as error:
        report['state'] = (op.d.record or {}).get('state', 'failed_before_execution')
        report['error'] = public_error(error)['message']
    finally:
        from platform_release import public_stages
        report['stages'] = public_stages(op.d.record or {})
        output.parent.mkdir(parents=True, exist_ok=True)
        durable_json(output, report)
    print(json.dumps(dict(report, report_path=str(output)), ensure_ascii=False))
    if report['state'] != 'succeeded':
        raise SystemExit(1)


if __name__ == '__main__':
    main()
