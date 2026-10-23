"""Durable, single-target lower release. No automatic replay or data restore.

This module is deliberately separate from build and legacy/manual deployment.
Agent owns process operations; Rust owns admission. The journal is evidence,
not permission to infer that an ambiguous remote operation never happened.
"""
from contextlib import contextmanager
from datetime import datetime, timezone
import hashlib
import errno
import json
import os
from pathlib import Path
import re
import stat
import time
import uuid


def now():
    return datetime.now(timezone.utc).isoformat()


def sync_directory(directory):
    fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def durable_json(path, data):
    temporary = path.with_name(path.name + '.' + uuid.uuid4().hex + '.tmp')
    try:
        with temporary.open('x', encoding='utf-8') as stream:
            mode = 0o600
            # Root-run publishers and the unprivileged Agent share only these
            # small intent markers, not private deployment journals/configs.
            if path.name == '.rcd-host-release-active.json' or path.name.endswith('.release-active.json'):
                control = path.parent / '.rcd-host-control'
                if control.exists():
                    info = control.lstat()
                    if not stat.S_ISDIR(info.st_mode):
                        raise RuntimeError('host control directory must not be a symlink')
                    if os.geteuid() == 0:
                        os.fchown(stream.fileno(), -1, info.st_gid)
                    elif os.getegid() != info.st_gid:
                        raise RuntimeError('publisher cannot provision shared marker permissions')
                    mode = 0o640
                    preserve_shared_marker_kid(path, temporary, control)
            os.chmod(temporary, mode)
            json.dump(data, stream, ensure_ascii=False, indent=2)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        sync_directory(path.parent)
    finally:
        temporary.unlink(missing_ok=True)


def preserve_shared_marker_kid(path, temporary, control):
    """Preserve an administrator-provisioned KYSEC KID on atomic replacement.

    Do not copy executable trust flags or change any global security policy.
    Missing KYSEC support is normal on other Linux hosts; permission failures
    are not, and must stop publication before replacing the old marker.
    """
    def label(target):
        try:
            return os.getxattr(target, 'security.ksip', follow_symlinks=False)
        except OSError as error:
            if error.errno in (errno.ENODATA, errno.ENOTSUP):
                return None
            raise

    if path.is_symlink():
        raise RuntimeError('shared marker must not be a symlink')
    existing = label(path if path.exists() else control)
    if existing is None:
        return
    fresh = label(temporary)
    if fresh is None:
        raise RuntimeError('KYSEC shared marker label missing on temporary file')
    old_parts, new_parts = existing.split(b':'), fresh.split(b':')
    if len(old_parts) != 4 or len(new_parts) != 4 or not old_parts[-1].isdigit():
        raise RuntimeError('unrecognized KYSEC shared marker label')
    expected = b':'.join(new_parts[:3] + old_parts[-1:])
    if fresh != expected:
        os.setxattr(temporary, 'security.ksip', expected, follow_symlinks=False)
        if label(temporary) != expected:
            raise RuntimeError('KYSEC shared marker label verification failed')


def fingerprint(profile):
    # Do not archive profile contents: they may contain environment secrets.
    return hashlib.sha256(json.dumps(profile, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


def marker_path(release):
    return release.entry.parent / ('.' + release.entry.name + '.release-active.json')


def host_directory(release):
    # Release validates the only override as /opt/rcd/bin for registered apps.
    return Path(getattr(release, 'p', {}).get('host_lock_dir', release.entry.parent))


def require_no_unfinished_release(release, allow_agent_recovery=False):
    from release_probe import read_json
    lifecycle = host_directory(release) / '.rcd-host-control' / 'lifecycle.json'
    if lifecycle.exists():
        data = read_json(lifecycle, 65536)
        if data.get('state') not in ('succeeded', 'failed'):
            saved = read_json(release.runtime / 'process-state.json', 8 << 20) if allow_agent_recovery and release.p['component'] == 'agent' else {}
            if not saved.get('lifecycle') or saved['lifecycle'].get('request_id') != data.get('request_id'):
                raise RuntimeError('unfinished host lifecycle operation; reconcile the original control request')
            # Recovering this exact stopped owner does not resolve or replay its
            # lower job. The recovered Agent loads it as unknown/admission closed.
    host_marker = host_directory(release) / '.rcd-host-release-active.json'
    if host_marker.exists():
        data = read_json(host_marker, 65536)
        if data.get('state') not in ('succeeded', 'recovered', 'failed_before_install'):
            raise RuntimeError('unfinished host release; reconcile the original operation before any component upgrade')
    marker = marker_path(release)
    if marker.exists():
        data = read_json(marker, 65536)
        if data.get('state') != 'succeeded':
            raise RuntimeError('unfinished release; use inspect-deployment, do not replay or roll back blindly')


@contextmanager
def target_lock(release):
    # All three fixed /opt/rcd/bin entries share the host release lock. Keep the
    # legacy component lock too, so older release runners still contend for the
    # same binary. Install the updated runner at every entry point before Mender
    # admission: an old runner cannot know about the new cross-component lock.
    # Never unlink a flock inode while waiters exist.
    import fcntl
    import stat
    paths = [host_directory(release) / '.rcd-host-release.lock',
             release.entry.parent / ('.' + release.entry.name + '.release.lock')]
    descriptors = []
    try:
        for path in paths:
            fd = os.open(path, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600)
            descriptors.append(fd)
            info = os.fstat(fd)
            if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
                raise RuntimeError('release lock must be a regular non-linked file')
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        yield
    finally:
        for fd in reversed(descriptors):
            os.close(fd)


class Deployment:
    def __init__(self, release, deployment_id):
        if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,95}', deployment_id):
            raise ValueError('explicit safe deployment-id (1-96 characters) required')
        self.release = release
        self.id = deployment_id
        self.directory = release.root / 'deployments' / deployment_id
        self.path = self.directory / 'deployment.json'
        self.record = None
        self.token = None

    def save(self):
        self.record['updated_at'] = now()
        durable_json(self.path, self.record)

    def step(self, name, function, intent=None):
        event = {'stage': name, 'started_at': now(), 'state': 'intent', 'intent': intent or {}}
        self.record['stages'].append(event)
        self.save()  # If intent cannot be persisted, never execute the action.
        try:
            result = function()
        except BaseException as error:
            # No remote response/body/exception text in durable logs; it could
            # contain credentials/config contents. An ambiguous effect stays so.
            event.update(state='unknown', ended_at=now(), error_type=type(error).__name__)
            self.record['state'] = 'unknown'
            self.save()
            raise
        event.update(state='confirmed', ended_at=now(), result=result)
        self.save()
        return result

    def process_snapshot(self):
        r = self.release
        state = r.json_request('/api/v1/process')
        if (state.get('instance_id') != r.p['control']['instance_id'] or
                state.get('executable') != str(r.entry) or
                state.get('work_dir') != r.p.get('work_dir', str(r.runtime)) or
                not isinstance(state.get('revision'), str) or not state['revision']):
            raise RuntimeError('agent process binding or revision mismatch')
        return state

    def maintenance(self):
        state = self.release.json_request('/api/v1/maintenance')
        if state.get('supported') is not True or not isinstance(state.get('revision'), str) or not state['revision']:
            raise RuntimeError('agent maintenance unsupported or malformed; no fallback')
        if (state.get('gateway_instance_id') != self.release.p['gateway_instance_id'] or
                state.get('endpoint') != self.release.p['maintenance_endpoint']):
            raise RuntimeError('configured agent/lower binding mismatch; no maintenance requested')
        return state

    def owner(self, state):
        job = state.get('job') or {}
        p = self.release.p
        if (job.get('deployment_id') != self.id or job.get('instance_id') != p['control']['instance_id'] or
                job.get('gateway_instance_id') != p['gateway_instance_id'] or
                job.get('endpoint') != p['maintenance_endpoint'] or
                job.get('target_version') != self.record['target_version']):
            raise RuntimeError('maintenance ownership or target mismatch')
        token = job.get('fencing_token', '')
        if (not isinstance(token, str) or not re.fullmatch(r'[1-9][0-9]{0,19}', token) or
                int(token) > 18446744073709551615 or (self.token and token != self.token)):
            raise RuntimeError('maintenance generation mismatch')
        self.token = token
        return job

    def job(self, state, completed=False):
        job = self.owner(state)
        token = self.token
        p = self.release.p
        lower = job.get('lower') or {}
        if (lower.get('deployment_id') != self.id or lower.get('fencing_token') != token or
                lower.get('gateway_instance_id') != p['gateway_instance_id'] or
                lower.get('durable') is not True or not lower.get('boot_id') or lower.get('error')):
            raise RuntimeError('missing durable lower evidence')
        if completed:
            if (job.get('phase') != 'completed' or lower.get('active') is not False or
                    lower.get('admission_blocked') is not False):
                raise RuntimeError('maintenance completion not confirmed')
        elif (job.get('lower_confirmed') is not True or job.get('release_requested') is not False or
              lower.get('active') is not True or lower.get('admission_blocked') is not True):
            raise RuntimeError('lower gate not confirmed')
        # Only allowlisted operational evidence, no configuration or error body.
        return {k: lower.get(k) for k in ('gateway_instance_id', 'boot_id', 'deployment_id', 'fencing_token',
                                         'active', 'admission_blocked', 'durable', 'drained',
                                         'pending_admissions', 'pending_port_operations')}

    def maintenance_action(self, action):
        state = self.maintenance()
        if action != 'enter':
            # After start/restart the cached boot may be old; reconciliation
            # below obtains fresh evidence, without replaying a process action.
            self.job(state)
        elif state.get('job') and state['job'].get('phase') != 'completed':
            raise RuntimeError('agent already owns a maintenance job')
        request = {'action': action, 'instance_id': self.release.p['control']['instance_id'],
                   'deployment_id': self.id, 'expected_revision': state['revision']}
        if action == 'enter':
            request.update(target_version=self.record['target_version'], initiator=self.record['initiator'])
        result = self.release.json_request('/api/v1/maintenance/actions', request)
        return self.job(result, completed=action == 'complete')

    def drain(self):
        end = time.monotonic() + 120
        while time.monotonic() < end:
            evidence = self.maintenance_action('reconcile')
            if evidence['drained'] is True and evidence['pending_admissions'] == 0 and evidence['pending_port_operations'] == 0:
                return evidence
            time.sleep(0.5)
        raise RuntimeError('lower did not drain; left in maintenance')

    def process_action(self, action, request_id=None):
        state = self.process_snapshot()
        if state.get('pending_request_id'):
            raise RuntimeError('process action already pending')
        request = {'request_id': request_id or self.id + '.' + action, 'action': action,
                   'instance_id': state['instance_id'], 'expected_revision': state['revision'],
                   'deployment_id': self.id, 'fencing_token': self.token}
        # Exactly one submission. A lost POST response is unknown, never a
        # reason to issue a second POST (or fall back to ordinary control).
        result = self.release.json_request('/api/v1/process/actions', request)
        end = time.monotonic() + 120
        while True:
            if any(result.get(k) != request[k] for k in ('request_id', 'instance_id', 'action')):
                raise RuntimeError('process operation identity mismatch')
            if result.get('state') == 'succeeded':
                current = self.process_snapshot()
                expected = 'stopped' if action == 'stop' else 'running'
                if (current.get('state') != expected or current.get('desired_state') != expected or
                        current.get('pending_request_id') or (action == 'stop' and current.get('pid', 0) != 0)):
                    raise RuntimeError('terminal operation disagrees with current process')
                return {'request_id': request['request_id'], 'state': 'succeeded', 'pid': current.get('pid', 0)}
            if result.get('state') not in ('accepted', 'running') or time.monotonic() >= end:
                raise RuntimeError('process result failed or unknown; inspect, do not replay')
            time.sleep(0.5)
            result = self.release.json_request('/api/v1/process/operations/' + request['request_id'])

    def deploy(self, artifact, initiator):
        from release import atomic_copy, sha
        r = self.release
        r.local_host()
        if (r.p['component'] != 'lower' or r.p.get('control', {}).get('kind') != 'process-agent' or
                not r.p.get('gateway_instance_id') or not r.p.get('maintenance_endpoint')):
            raise ValueError('fenced deployment requires an explicitly bound lower/process-agent profile')
        if r.entry.is_symlink() or not initiator.strip() or len(initiator) > 128:
            raise ValueError('fixed regular binary path and initiator required')
        with target_lock(r):
            require_no_unfinished_release(r)
            if self.directory.exists():
                raise RuntimeError('deployment ID already recorded; inspect only, never replay')
            # All preflight errors are before remote mutation/active marker.
            r.preflight()
            manifest = r.verify(artifact)
            version = manifest.get('build_info', {}).get('version', '')
            if not re.fullmatch(r'[A-Za-z0-9_.-]{1,128}', version):
                raise ValueError('artifact must have an agent-compatible recorded software version')
            self.process_snapshot()
            current = self.maintenance()
            if current.get('job') and current['job'].get('phase') != 'completed':
                raise RuntimeError('another agent maintenance job exists')
            self.directory.mkdir(parents=True, mode=0o700)
            sync_directory(self.directory.parent)
            sync_directory(r.root)
            self.record = {'schema_version': 1, 'deployment_id': self.id, 'state': 'running',
                           'host': r.p['host'], 'component': 'lower', 'executable': str(r.entry),
                           'instance_id': r.p['control']['instance_id'], 'profile_sha256': fingerprint(r.p),
                           'artifact': str(artifact), 'binary_sha256': manifest['binary_sha256'],
                           'manifest_sha256': sha(artifact / 'manifest.json'), 'target_version': version,
                           'previous_binary_sha256': sha(r.entry),
                           'initiator': initiator, 'created_at': now(), 'stages': []}
            self.save()
            marker = {'deployment_id': self.id, 'record': str(self.path), 'state': 'running'}
            durable_json(marker_path(r), marker)
            self.step('maintenance_enter', lambda: self.maintenance_action('enter'))
            self.step('drained', self.drain)
            self.step('stopped', lambda: self.process_action('stop'), {'request_id': self.id + '.stop'})
            backup = self.step('backed_up', lambda: {'path': str(r.backup(artifact))})
            def install():
                # Re-check artifact and stopped state after potentially slow backup.
                if r.verify(artifact) != manifest or sha(artifact / 'manifest.json') != self.record['manifest_sha256']:
                    raise RuntimeError('artifact changed after staging')
                state = self.process_snapshot()
                if state.get('pid', 0) != 0 or state.get('state') != 'stopped' or state.get('pending_request_id'):
                    raise RuntimeError('lower no longer stopped')
                atomic_copy(artifact / 'binary', r.entry, executable=True)
                if sha(r.entry) != manifest['binary_sha256']:
                    raise RuntimeError('installed hash mismatch')
                return {'binary_sha256': manifest['binary_sha256']}
            self.step('installed', install)
            self.step('started', lambda: self.process_action('start'), {'request_id': self.id + '.start'})
            accepted = self.step('software_acceptance', lambda: r.accept(manifest['binary_sha256']))
            self.step('lower_verified', self.drain)  # Fresh OPC, identity + retained fence after boot.
            self.step('maintenance_completed', lambda: self.maintenance_action('complete'))
            self.record['state'] = 'succeeded'
            self.save()
            marker['state'] = 'succeeded'
            durable_json(marker_path(r), marker)
            return {'deployment_record': str(self.path), 'recovery_backup': backup['path'],
                    'software_acceptance': accepted, 'business_acceptance': 'not_performed'}

    def inspect(self):
        """Read-only reconciliation: no start/stop/enter/leave/config restoration."""
        from release import sha
        r = self.release
        r.local_host()
        record = json.loads(self.path.read_text())
        if (record.get('deployment_id') != self.id or record.get('host') != r.p['host'] or
                record.get('component') != r.p['component'] or record.get('executable') != str(r.entry) or
                record.get('profile_sha256') != fingerprint(r.p)):
            raise RuntimeError('journal/profile binding changed; explicit recovery review required')
        result = {'record': str(self.path), 'recorded_state': record['state'],
                  'outcome': record.get('outcome'), 'rollback': record.get('rollback'),
                  'stages': record['stages'], 'entry_sha256': sha(r.entry), 'observed_at': now(),
                  'recovery_attempts': record.get('recovery_attempts', []),
                  'resume_checkpoints': record.get('resume_checkpoints', []),
                  'automatic_resume': False, 'agent_evidence': 'cached; no OPC reconciliation performed'}
        try:
            artifact = Path(record['artifact'])
            manifest = r.verify(artifact)
            result['artifact_unchanged'] = (manifest['binary_sha256'] == record['binary_sha256'] and
                                            sha(artifact / 'manifest.json') == record['manifest_sha256'])
        except Exception as error:
            result['artifact_observation_error'] = type(error).__name__
        for label, endpoint in [('process', '/api/v1/process'), ('maintenance', '/api/v1/maintenance'),
                                ('stop', '/api/v1/process/operations/' + self.id + '.stop'),
                                ('rollback_start', '/api/v1/process/operations/' + self.id + '.rollback-start'),
                                ('start', '/api/v1/process/operations/' + self.id + '.start')]:
            try:
                value = self.process_snapshot() if label == 'process' else r.json_request(endpoint)
                if label == 'process' and int(value.get('pid') or 0) > 1:
                    image = Path('/proc') / str(int(value['pid'])) / 'exe'
                    if image.resolve() != r.entry.resolve():
                        raise RuntimeError('running image does not match target')
                    result['running_binary_sha256'] = sha(image)
                # Keep only diagnostic states/IDs, never error strings or config.
                if label == 'maintenance':
                    value = value.get('job') or {}
                result[label] = {k: value[k] for k in ('state', 'phase', 'pid', 'revision', 'deployment_id',
                                                       'fencing_token', 'request_id', 'instance_id',
                                                       'pending_request_id', 'lower_confirmed') if k in value}
            except Exception as error:
                result[label] = {'observation_error': type(error).__name__}
        return result
