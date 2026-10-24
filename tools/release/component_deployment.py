"""Binary-only component transactions using the existing RCD publisher.

The package declares compatibility; the installed profile fixes every path.
No database/config restoration, remote shell payload, or command replay exists.
"""
from contextlib import closing, ExitStack
import http.client
import json
import os
from pathlib import Path
import re
import socket
import subprocess
import time

from deployment import Deployment, durable_json, fingerprint, now, require_no_unfinished_release, target_lock, host_directory, sync_directory
from release import Release, SoftwareAcceptanceError, atomic_copy, sha
from release_probe import ADAPTER_VERSION, ReleaseProblem, read_formats, read_json, public_error



def version(value):
    match = re.fullmatch(r'(\d+)\.(\d+)\.(\d+)(?:[-+].*)?', value)
    if not match:
        raise ValueError('comparable component version required')
    return tuple(int(v) for v in match.groups())


def state_versions(r):
    return read_formats(r.p)


def compatible(r, manifest):
    c = manifest.get('compatibility', {})
    if version(c.get('adapter_min_version', '')) > version(ADAPTER_VERSION):
        raise ReleaseProblem('adapter_incompatible', '升级适配器版本过低，需要先更新维护工具')
    actual = state_versions(r)
    bounds = c.get('state_formats', {})
    if set(actual) != set(bounds):
        raise ReleaseProblem('state_inventory_invalid', '包声明的数据格式与目标数据库绑定不一致')
    for name, current in actual.items():
        if not bounds[name]['min'] <= current <= bounds[name]['max']:
            raise ReleaseProblem('state_incompatible', 'incompatible current state format: ' + name + '；该版本不能读取当前数据，禁止恢复')
    # Dependencies use trusted installed identities, not arbitrary binaries.
    for component, minimum in c.get('component_min_versions', {}).items():
        path = r.p.get('component_identity_files', {}).get(component)
        if not path:
            raise RuntimeError('missing dependency identity: ' + component)
        identity = json.loads(Path(path).read_text())
        binary = r.p.get('component_executables', {}).get(component)
        if not binary or sha(Path(binary)) != identity.get('binary_sha256') or version(identity['build_info']['version']) < version(minimum):
            raise RuntimeError('component dependency not satisfied: ' + component)
    return actual


class UnixHTTP(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__('localhost', timeout=8)
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)


class ComponentDeployment(Deployment):
    """Shares old validation/backup/control primitives, with one durable journal."""
    def __init__(self, release, deployment_id):
        super().__init__(release, deployment_id)
        self.host_marker = host_directory(release) / '.rcd-host-release-active.json'
        self.lower = None
        self.fully_stopped_agent = False
        self.offline_state_hash = None
        self.report_binding = None
        if release.p['component'] == 'lower':
            self.lower = Deployment(release, deployment_id)
        elif release.p['component'] == 'agent':
            profile = json.loads(Path(release.p['lower_profile']).read_text())
            lower = Release(profile, release.p['host'], 'lower')
            if lower.entry.parent != release.entry.parent:
                raise ValueError('Agent and lower must share a host release lock')
            self.lower = Deployment(lower, deployment_id)

    def step(self, name, function, intent=None):
        try:
            return super().step(name, function, intent)
        except Exception as error:
            self.record['stages'][-1]['error'] = public_error(error, name)
            self.save()
            raise

    def manager_request(self, payload=None):
        path = str(self.release.runtime / 'release-control.sock')
        with closing(UnixHTTP(path)) as conn:
            headers = {}
            raw = None
            if payload is not None:
                raw = json.dumps(payload)
                headers = {'Content-Type': 'application/json', 'If-Match': '"' + str(payload['expected_revision']) + '"'}
            conn.request('POST' if payload else 'GET', '/release', raw, headers)
            response = conn.getresponse()
            body = response.read(65537)
            if response.status != 200 or len(body) > 65536:
                raise RuntimeError('manager release control refused request')
            return json.loads(body)

    def manager_action(self, action):
        before = self.manager_request()
        identity = self.record.setdefault('manager_instance_id', before['instance_id'])
        if before['instance_id'] != identity or (before['active'] and before['deployment_id'] != self.id):
            raise RuntimeError('manager maintenance owner mismatch')
        self.save()
        result = self.manager_request({'action': action, 'instance_id': identity,
                                      'deployment_id': self.id, 'expected_revision': before['revision']})
        if result['deployment_id'] != self.id or result['active'] != (action == 'enter'):
            raise RuntimeError('manager maintenance response mismatch')
        return result

    def offline_action(self, action):
        d = self.lower
        state = d.process_snapshot()
        if state.get('pid') or state['state'] != 'stopped' or state['desired_state'] != 'stopped' or state.get('pending_request_id'):
            raise RuntimeError('lower stopped state cannot be confirmed')
        m = d.release.json_request('/api/v1/maintenance')
        req = {'action': action, 'instance_id': d.release.p['control']['instance_id'],
               'deployment_id': self.id, 'expected_revision': m['revision'],
               'target_version': self.record['target_version'], 'initiator': self.record['initiator']}
        result = d.release.json_request('/api/v1/maintenance/actions', req)
        job = result.get('job') or {}
        if job.get('deployment_id') != self.id or job.get('offline_hold') is not True or job.get('phase') != ('completed' if action == 'release-stopped' else 'stopped'):
            raise RuntimeError('offline hold response mismatch')
        return {'deployment_id': self.id, 'offline_hold': True, 'phase': job['phase']}

    def enter(self):
        if self.release.application:
            self.release.require_application_service()
            hold = self.release.runtime / 'manual-stop.json'
            if hold.exists() or hold.is_symlink():
                saved = read_json(hold)
                if self.record['was_running'] or saved.get('desired_state') != 'stopped':
                    raise RuntimeError('application stop protection conflicts with the observed process state')
                self.record['application_existing_stop_hold'] = True
                self.record['application_existing_stop_hold_sha256'] = sha(hold)
            else:
                self.record['application_existing_stop_hold'] = False
                durable_json(hold, self.application_release_hold())
            self.save()
            return {'normal_process_stop_only': True, 'device_transaction_drain': False, 'boot_stop_protected': True}
        if self.release.p['component'] == 'communicate':
            # No reliable runtime admission/drain interface exists yet. Never
            # treat it as the manager socket or stop its unrelated lower Agent.
            if self.record['was_running'] or self.release.pid():
                raise ReleaseProblem('communicate_drain_unsupported', '通信程序尚不支持可信的活动通信排空；须先由维护人员受控停止，不能自动停止或重启')
            self.release.require_stopped_communicate()
            return {'already_stopped': True, 'kept_stopped': True}
        if self.fully_stopped_agent:
            self.verify_offline_agent()
            return {'kept_stopped': True, 'agent_instance_lock': True}
        if self.lower:
            self.lower.record = self.record
            if not self.record['lower_was_running']:
                return self.offline_action('hold-stopped')
            self.lower.maintenance_action('enter')
            self.lower.drain()
            result = self.lower.process_action('stop')
            self.record['fencing_token'] = self.lower.token
            self.save()
            return result
        if not self.record['was_running']:
            return {'already_stopped': True}
        self.manager_action('enter')
        end = time.monotonic() + 120
        while time.monotonic() < end:
            value = self.manager_request()
            if value['deployment_id'] != self.id or not value['active']:
                raise RuntimeError('manager maintenance ownership lost')
            if value['drained']:
                return value
            time.sleep(.25)
        raise RuntimeError('manager commands did not drain')

    def stopped(self):
        if self.release.pid() != 0:
            raise RuntimeError('target process is not stopped')
        if self.release.application:
            self.release.require_application_service()
            self.ensure_no_process_images({str(self.release.entry)})
            if self.record and 'application_existing_stop_hold' in self.record:
                hold = self.release.runtime / 'manual-stop.json'
                saved = read_json(hold)
                if self.record['application_existing_stop_hold']:
                    valid = saved.get('desired_state') == 'stopped' and sha(hold) == self.record['application_existing_stop_hold_sha256']
                else:
                    valid = saved == self.application_release_hold()
                if not valid:
                    raise RuntimeError('application boot stop protection changed during release')
        if self.release.p['component'] == 'communicate':
            self.release.require_stopped_communicate()
        if self.lower:
            if self.fully_stopped_agent:
                self.verify_offline_agent()
                return
            if self.release.p['component'] == 'agent':
                state = json.loads((self.release.runtime / 'process-state.json').read_text())
                job = state.get('maintenance') or {}
                if (state.get('instance_id') != self.lower.release.p['control']['instance_id'] or
                        state.get('desired_state') != 'stopped' or job.get('deployment_id') != self.id or
                        job.get('phase') == 'completed' or state.get('last_snapshot', {}).get('pid', 0)):
                    raise RuntimeError('stopped Agent/lower durable identity mismatch')
                return
            s = self.lower.process_snapshot()
            failed_request = (self.record or {}).get('failed_lower_start_request')
            if failed_request and self.confirmed_lower_start_failed(failed_request):
                return
            if s.get('pid') or s['state'] != 'stopped' or s.get('pending_request_id'):
                raise RuntimeError('lower is no longer stopped')

    def restore_running(self, expected):
        r = self.release
        if r.application and self.record['was_running']:
            return self.restore_running_application(expected)
        if r.p['component'] != 'lower' and self.record['was_running']:
            try:
                r.control('start')
            except subprocess.CalledProcessError as error:
                # A local service manager rejection is not a lost HTTP reply.
                # Only a fresh, definitive stopped state can enter recovery.
                if self.confirmed_service_stopped():
                    raise SoftwareAcceptanceError('confirmed local startup failure') from error
                raise
        if self.lower and not self.fully_stopped_agent:
            # Agent restart changes its revision/phase, never the fence identity.
            end = time.monotonic() + 60
            while True:
                try:
                    state = self.lower.process_snapshot()
                    break
                except (OSError, ValueError):
                    if time.monotonic() >= end:
                        if r.p['component'] == 'agent' and self.confirmed_service_stopped():
                            raise SoftwareAcceptanceError('Agent stopped before readiness')
                        raise
                    time.sleep(.5)
            if self.record['lower_was_running']:
                self.lower.token = self.record.get('fencing_token')
                request_id = self.id + ('.rollback-start' if expected != self.record['binary_sha256'] else '.start')
                try:
                    self.lower.process_action('start', request_id)
                except (OSError, ValueError, RuntimeError) as error:
                    if self.confirmed_lower_start_failed(request_id):
                        self.record['failed_lower_start_request'] = request_id
                        self.save()
                        raise SoftwareAcceptanceError('lower start confirmed failed, no child') from error
                    raise
            elif state['state'] != 'stopped' or state['desired_state'] != 'stopped':
                raise RuntimeError('previously stopped lower was started')
        if self.record['was_running']:
            return r.accept(expected)
        self.stopped()
        if sha(r.entry) != expected: raise RuntimeError('installed stopped binary hash mismatch')
        return {'binary_sha256': expected, 'state': 'installed_stopped', 'pid': 0}

    def application_release_hold(self):
        return {'schema_version': 1, 'desired_state': 'stopped', 'reason': 'component_upgrade',
                'operation_id': self.id, 'component': self.release.p['component']}

    def restore_running_application(self, expected):
        r = self.release
        self.stopped()
        hold = r.runtime / 'manual-stop.json'
        if read_json(hold) != self.application_release_hold() or self.record.get('application_existing_stop_hold'):
            raise RuntimeError('application release stop protection changed; refusing automatic startup')
        # The acceptance stage's durable intent precedes this sub-intent. A
        # stopped process never loses its boot protection during backup/install;
        # only the explicitly recorded start of this exact image removes it.
        self.record['application_start_image_sha256'] = expected
        self.save()
        hold.unlink()
        sync_directory(hold.parent)
        try:
            r.control('start')
            return r.accept(expected)
        except BaseException as error:
            # Unknown/startup-failed results retain a boot inhibit as well as
            # the journal. Do not stop/retry a process just to manufacture proof.
            if not hold.exists() and not hold.is_symlink():
                durable_json(hold, self.application_release_hold())
            if isinstance(error, subprocess.CalledProcessError) and self.confirmed_service_stopped():
                raise SoftwareAcceptanceError('confirmed local application startup failure') from error
            raise

    def complete(self):
        if self.release.application:
            return {'normal_process_stop_only': True, 'kept_stopped': not self.record['was_running']}
        if self.release.p['component'] == 'communicate':
            self.stopped()
            return {'kept_stopped': True}
        if self.fully_stopped_agent:
            self.verify_offline_agent()
            return {'kept_stopped': True, 'state_unchanged': True}
        if self.lower:
            if self.record['lower_was_running']:
                self.lower.drain()
                return self.lower.maintenance_action('complete')
            return self.offline_action('release-stopped')
        if self.record['was_running']:
            return self.manager_action('complete')
        return {'kept_stopped': True}

    def recover_binary(self):
        r = self.release
        backup = Path(self.record['backup'])
        if sha(backup / 'binary') != self.record['previous_binary_sha256']:
            raise RuntimeError('previous binary backup damaged')
        # Compare against the pre-maintenance state too: schema-3 stopped holds
        # must never be fed to a prior Agent without explicit compatibility proof.
        if state_versions(r) != self.record['initial_schemas']:
            raise RuntimeError('state format changed; automatic binary recovery refused')
        lower_already_dead = (self.lower and self.record.get('failed_lower_start_request')
                              and self.confirmed_lower_start_failed(self.record['failed_lower_start_request']))
        if self.lower and self.record['lower_was_running'] and r.p['component'] == 'agent':
            lower_already_dead = self.confirmed_dead_agent_holds_stopped_lower()
        if self.lower and self.record['lower_was_running'] and not lower_already_dead:
            self.lower.drain()
            self.lower.process_action('stop', self.id + '.rollback-stop')
        if r.p['component'] != 'lower':
            r.control('stop')
        self.stopped()
        atomic_copy(backup / 'binary', r.entry, executable=True)
        result = self.restore_running(self.record['previous_binary_sha256'])
        self.complete()
        self.mark('recovered')
        return {'state': 'recovered', 'acceptance': result, 'deployment_id': self.id}

    def mark(self, state):
        self.record['state'] = state
        self.save()
        durable_json(self.host_marker, {'deployment_id': self.id, 'component': self.release.p['component'],
                                      'record': str(self.path), 'state': state})

    def confirmed_service_stopped(self):
        r = self.release
        if r.p.get('control', {}).get('kind') not in ('systemd-user', 'systemd-system'):
            return False
        try:
            output = r.systemctl('show', r.p['control']['unit'], '--property=LoadState,ActiveState,SubState,MainPID,Job')
            values = dict(line.split('=', 1) for line in output.splitlines() if '=' in line)
            return (values.get('LoadState') == 'loaded' and values.get('MainPID') == '0' and values.get('ActiveState') in ('inactive', 'failed')
                    and values.get('SubState') in ('dead', 'failed') and values.get('Job') in ('', '0'))
        except (OSError, ValueError, subprocess.CalledProcessError):
            return False

    def confirmed_lower_start_failed(self, request_id):
        d = self.lower
        if d is None: return False
        try:
            result = d.release.json_request('/api/v1/process/operations/' + request_id)
            state = d.process_snapshot()
            job = d.owner(d.maintenance())
            return (result.get('request_id') == request_id and result.get('action') == 'start'
                    and result.get('instance_id') == d.release.p['control']['instance_id']
                    and result.get('state') == 'failed' and state.get('pid', 0) == 0
                    and state.get('state') == 'failed' and not state.get('pending_request_id')
                    and job.get('lower_confirmed') is True and job.get('release_requested') is False)
        except (OSError, ValueError, RuntimeError, KeyError):
            return False

    def confirmed_dead_agent_holds_stopped_lower(self):
        # If the replacement Agent never came up, HTTP drain cannot work.
        # Only durable ownership plus definitive process absence permits recovery.
        if not self.confirmed_service_stopped():
            return False
        self.stopped()
        state = read_json(self.release.runtime / 'process-state.json', 4 << 20)
        snapshot = state.get('last_snapshot', {})
        self.lower.token = self.record.get('fencing_token')
        job = self.lower.owner({'job': state.get('maintenance')})
        if (snapshot.get('state') != 'stopped' or snapshot.get('pending_request_id')
                or job.get('lower_confirmed') is not True or job.get('release_requested') is not False
                or any(v.get('operation', {}).get('state') in ('accepted', 'running')
                       for v in state.get('operations', []))):
            raise ReleaseProblem('agent_recovery_unconfirmed', 'Agent 启动失败，但下级停止或维护归属无法确认，禁止自动恢复')
        self.ensure_no_process_images({str(self.release.entry), str(self.lower.release.entry)})
        return True

    def lock_offline_agent(self, stack):
        import fcntl
        import stat
        if not self.confirmed_service_stopped():
            raise ReleaseProblem('agent_service_unconfirmed', 'Agent 服务仍在启动、重启或状态不明，禁止替换')
        path = self.release.runtime / 'agent.lock'
        fd = os.open(path, os.O_RDWR | os.O_NOFOLLOW | os.O_NONBLOCK)
        stack.callback(os.close, fd)
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
            raise ReleaseProblem('agent_lock_invalid', 'Agent 实例锁不是独立普通文件')
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        self.offline_state_hash = sha(self.release.runtime / 'process-state.json')
        self.fully_stopped_agent = True
        self.verify_offline_agent()

    def verify_offline_agent(self):
        r = self.release
        state = read_json(r.runtime / 'process-state.json', 4 << 20)
        snapshot = state.get('last_snapshot', {})
        job = state.get('maintenance') or {}
        if (r.pid() or sha(r.runtime / 'process-state.json') != self.offline_state_hash
                or state.get('instance_id') != self.lower.release.p['control']['instance_id']
                or state.get('desired_state') != 'stopped' or snapshot.get('pid', 0)
                or snapshot.get('state') != 'stopped' or snapshot.get('pending_request_id')
                or (job and job.get('phase') != 'completed')
                or (state.get('lifecycle') and state['lifecycle'].get('state') not in ('succeeded', 'failed'))
                or any(v.get('operation', {}).get('state') in ('accepted', 'running') for v in state.get('operations', []))):
            raise ReleaseProblem('agent_state_unknown', 'Agent 或下级停止状态无法确认，保留原状态，不执行升级')
        # A stale zero PID is insufficient; also reject a surviving/orphan child.
        targets = {str(r.entry), str(self.lower.release.entry)}
        self.ensure_no_process_images(targets)

    def ensure_no_process_images(self, targets):
        for process in Path('/proc').iterdir():
            if not process.name.isdigit():
                continue
            try:
                image = os.readlink(process / 'exe')
                if image.endswith(' (deleted)'): image = image[:-10]
                if image in targets:
                    raise ReleaseProblem('orphan_process', '发现仍在运行的 Agent 或下级映像，禁止覆盖程序')
            except (FileNotFoundError, ProcessLookupError):
                pass

    def deploy(self, artifact, initiator):
        r = self.release
        r.local_host()
        with target_lock(r), ExitStack() as stack:
            require_no_unfinished_release(r)
            if self.directory.exists(): raise RuntimeError('operation already recorded; inspect, never replay')
            preflight = r.preflight()
            if r.p['component'] == 'communicate' and preflight['pid']:
                raise ReleaseProblem('communicate_drain_unsupported', '通信程序运行中，缺少安全排空证明；拒绝安装且不停止原程序')
            manifest = r.verify(artifact)
            schemas = compatible(r, manifest)
            if r.p['component'] == 'agent' and not preflight['pid']:
                self.lock_offline_agent(stack)
            lower_state = ({'state': 'stopped', 'desired_state': 'stopped'} if self.fully_stopped_agent
                           else self.lower.process_snapshot() if self.lower else None)
            if lower_state and (lower_state.get('pending_request_id') or lower_state['state'] not in ('running', 'stopped') or lower_state['state'] != lower_state['desired_state']):
                raise RuntimeError('lower state is not confirmed')
            if lower_state and lower_state['state'] == 'stopped' and not self.fully_stopped_agent:
                # The offline hold raises the schema to 3. Do not install an old
                # Agent that would reject or silently forget that admission fence.
                if r.p['component'] == 'agent' and manifest['compatibility']['state_formats']['agent_state']['max'] < 3:
                    raise RuntimeError('target Agent does not support stopped maintenance format 3')
            self.directory.mkdir(parents=True, mode=0o700)
            self.record = {'schema_version': 1, 'deployment_id': self.id, 'state': 'running',
                'host': r.p['host'], 'component': r.p['component'], 'executable': str(r.entry),
                'profile_sha256': fingerprint(r.p), 'artifact': str(artifact),
                'binary_sha256': manifest['binary_sha256'], 'previous_binary_sha256': sha(r.entry),
                'target_version': manifest['build_info']['version'], 'initiator': initiator,
                'was_running': bool(preflight['pid']), 'lower_was_running': bool(lower_state and lower_state['state'] == 'running'),
                'initial_schemas': schemas, 'report_binding': self.report_binding, 'created_at': now(), 'stages': []}
            self.mark('running')
            installed = False
            try:
                self.step('maintenance', self.enter)
                if r.p['component'] != 'lower' and self.record['was_running']:
                    self.step('stopped', lambda: r.control('stop'))
                self.stopped()
                backup = self.step('backed_up', lambda: {'path': str(r.backup(artifact))})
                self.record['backup'] = backup['path']
                self.save()
                def install():
                    self.stopped()
                    if r.verify(artifact) != manifest: raise RuntimeError('package changed before install')
                    compatible(r, manifest)
                    atomic_copy(artifact / 'binary', r.entry, executable=True)
                    if sha(r.entry) != manifest['binary_sha256']: raise RuntimeError('installed hash mismatch')
                    return {'binary_sha256': manifest['binary_sha256']}
                self.step('installed', install)
                installed = True
                result = self.step('software_acceptance', lambda: self.restore_running(manifest['binary_sha256']))
                self.step('maintenance_completed', self.complete)
                self.mark('succeeded')
                return {'state': 'succeeded', 'acceptance': result, 'deployment_id': self.id}
            except SoftwareAcceptanceError:
                if not installed:
                    self.mark('needs_maintenance')
                    raise
                try:
                    return self.step('automatic_binary_recovery', self.recover_binary)
                except BaseException:
                    self.mark('recovery_failed')
                    raise
            except BaseException:
                # Uncertain RPC results must never cause repeat start/stop calls.
                # Automatic rollback is limited to confirmed local startup failures
                # and unchanged state schemas, otherwise retain all evidence.
                self.mark('unknown' if installed else 'needs_maintenance')
                raise
