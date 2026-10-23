"""Explicit post-start recovery. Never issues start/stop/enter or restores data."""
import json
from pathlib import Path

from deployment import Deployment, durable_json, fingerprint, marker_path, now, target_lock

ORDER = ['maintenance_enter', 'drained', 'stopped', 'backed_up', 'installed', 'started',
         'software_acceptance', 'lower_verified', 'maintenance_completed']


def effective_stages(record):
    """Read original history plus explicit recovery checkpoints, never rewrite it."""
    stages = list(record.get('stages', []))
    if not 3 <= len(stages) <= 9 or [s.get('stage') for s in stages] != ORDER[:len(stages)]:
        raise RuntimeError('invalid original deployment stage sequence')
    for checkpoint in record.get('resume_checkpoints', []):
        name = checkpoint.get('stage')
        if name not in ('stopped', 'backed_up', 'installed', 'started'):
            raise RuntimeError('invalid recovery checkpoint')
        index = ORDER.index(name)
        if index == len(stages):
            stages.append(checkpoint)
        elif index == len(stages) - 1 and stages[index].get('state') != 'confirmed':
            stages[index] = checkpoint
        else:
            raise RuntimeError('recovery checkpoint would overwrite confirmed history')
    if (any(s.get('state') != 'confirmed' for s in stages[:-1]) or
            any(s.get('state') not in ('intent', 'confirmed', 'unknown') for s in stages)):
        raise RuntimeError('invalid recovery stage state')
    return stages


class FinishDeployment(Deployment):
    def load_record(self):
        r = self.release
        self.record = json.loads(self.path.read_text())
        if (self.record.get('schema_version') != 1 or self.record.get('deployment_id') != self.id or
                self.record.get('host') != r.p['host'] or self.record.get('component') != 'lower' or
                r.p['component'] != 'lower' or r.p.get('control', {}).get('kind') != 'process-agent' or
                self.record.get('executable') != str(r.entry) or
                self.record.get('instance_id') != r.p['control']['instance_id'] or
                self.record.get('profile_sha256') != fingerprint(r.p)):
            raise RuntimeError('recovery journal/profile binding mismatch')
        marker = json.loads(marker_path(r).read_text())
        if marker.get('deployment_id') != self.id or marker.get('record') != str(self.path):
            raise RuntimeError('active marker belongs to another deployment')
        if self.record.get('state') == 'succeeded' and marker.get('state') == 'succeeded':
            raise RuntimeError('deployment already finished; use read-only inspection')
        if self.record.get('rollback') and not getattr(self, 'allow_rollback', False):
            raise RuntimeError('rollback intent exists; use finish-rollback, never resume the upgrade')
        self.stages = effective_stages(self.record)
        self.token = self.stages[0]['result']['fencing_token']

    def load(self):
        self.load_record()
        stages = self.stages
        if (not 6 <= len(stages) <= 9 or
                any(s.get('state') != 'confirmed' for s in stages[:5]) or
                any(s.get('state') != 'confirmed' for s in stages[:-1]) or
                any(s.get('state') not in ('intent', 'confirmed', 'unknown') for s in stages)):
            raise RuntimeError('recovery only supports confirmed installation and a recorded start attempt')
        if (stages[2].get('intent', {}).get('request_id') != self.id + '.stop' or
                stages[5].get('intent', {}).get('request_id') != self.id + '.start'):
            raise RuntimeError('original process request IDs are not recorded')
        self.complete_attempted = len(stages) == 9 or any(
            stage.get('stage') == 'complete' for attempt in self.record.get('recovery_attempts', [])
            for stage in attempt.get('stages', []))

    def step(self, name, function):
        event = {'stage': name, 'state': 'intent', 'started_at': now()}
        self.attempt['stages'].append(event)
        self.save()
        try:
            result = function()
        except BaseException as error:
            event.update(state='unknown', ended_at=now(), error_type=type(error).__name__)
            self.attempt['state'] = 'unknown'
            self.record['state'] = 'unknown'
            self.save()
            raise
        event.update(state='confirmed', ended_at=now(), result=result)
        self.save()
        return result

    def verify_artifact(self):
        from release import sha
        r = self.release
        artifact = Path(self.record['artifact'])
        manifest = r.verify(artifact)
        if (sha(artifact / 'manifest.json') != self.record['manifest_sha256'] or
                manifest['binary_sha256'] != self.record['binary_sha256']):
            raise RuntimeError('release artifact changed')
        return artifact

    def verify_backup(self, backup):
        from release import sha, within
        r = self.release
        artifact = self.verify_artifact()
        if not backup.is_absolute() or not within(backup, artifact) or backup.is_symlink():
            raise RuntimeError('backup path is outside original artifact')
        saved = json.loads((backup / 'backup.json').read_text())
        if (saved['host'] != r.p['host'] or saved['component'] != 'lower' or
                saved['executable'] != str(r.entry) or sha(backup / 'binary') != saved['binary_sha256']):
            raise RuntimeError('backup identity or binary damaged')
        allowed = {str(p): p in r.databases for p in r.databases + r.configs}
        files = saved['files']
        if len(files) != len(allowed) or {f['target'] for f in files} != set(allowed):
            raise RuntimeError('backup inventory mismatch')
        for item in files:
            file = backup / item['file']
            if (Path(item['file']).name != item['file'] or file.is_symlink() or
                    item['database'] != allowed[item['target']] or sha(file) != item['sha256']):
                raise RuntimeError('backup file damaged or outside inventory')
        # Current configuration/data is intentionally not compared to backup.
        return {'artifact_sha256': self.record['binary_sha256'], 'backup_verified': str(backup)}

    def verify_files(self):
        from release import sha
        self.verify_artifact()
        if self.release.entry.is_symlink() or sha(self.release.entry) != self.record['binary_sha256']:
            raise RuntimeError('installed binary changed')
        return self.verify_backup(Path(self.stages[3]['result']['path']))

    def verify_started(self):
        return self.verify_start_request(self.id + '.start')

    def verify_start_request(self, request_id):
        current = self.process_snapshot()
        if (current.get('state') != 'running' or current.get('desired_state') != 'running' or
                current.get('pending_request_id') or int(current.get('pid') or 0) <= 1):
            raise RuntimeError('lower not confirmed running; recovery never starts it')
        operation = self.release.json_request('/api/v1/process/operations/' + request_id)
        if (operation.get('request_id') != request_id or operation.get('action') != 'start' or
                operation.get('instance_id') != self.record['instance_id'] or operation.get('state') != 'succeeded'):
            raise RuntimeError('original start not confirmed; no retry (404 is not proof of non-execution)')
        return {'request_id': operation['request_id'], 'state': 'succeeded', 'pid': current['pid']}

    def reconcile(self):
        state = self.maintenance()
        self.owner(state)  # Validate owner/token even if cached lower evidence is unknown.
        result = self.release.json_request('/api/v1/maintenance/actions', {
            'action': 'reconcile', 'instance_id': self.record['instance_id'], 'deployment_id': self.id,
            'expected_revision': state['revision']})
        job = self.owner(result)
        completed = job.get('phase') == 'completed'
        if job.get('release_requested') and not completed:
            raise RuntimeError('release still unconfirmed; do not resend complete')
        evidence = self.job(result, completed=completed)
        if not completed and (evidence['drained'] is not True or evidence['pending_admissions'] != 0 or
                              evidence['pending_port_operations'] != 0):
            raise RuntimeError('lower not drained; retain maintenance')
        return dict(evidence, completed=completed)

    def acceptance_hash(self):
        return self.record['binary_sha256']

    def finish(self, initiator):
        r = self.release
        r.local_host()
        if not initiator.strip() or len(initiator) > 128:
            raise ValueError('recovery initiator required')
        with target_lock(r):
            self.load()
            attempts = self.record.setdefault('recovery_attempts', [])
            if len(attempts) >= 100:
                raise RuntimeError('recovery attempt limit reached; retain evidence for manual review')
            self.attempt = {'number': len(attempts) + 1, 'initiator': initiator, 'started_at': now(),
                            'state': 'running', 'stages': []}
            attempts.append(self.attempt)
            self.save()
            self.step('files_verified', self.verify_files)
            self.step('original_start_verified', self.verify_started)
            # No TCP-only conclusion: r.accept verifies the running /proc image,
            # then reconcile must perform a fresh OPC read, even if completed.
            accepted = self.step('software_acceptance', lambda: r.accept(self.acceptance_hash()))
            evidence = self.step('reconciled', self.reconcile)
            if not evidence['completed']:
                def allow_complete():
                    if self.complete_attempted:
                        raise RuntimeError('a prior complete intent exists; no ambiguous mutation replay')
                    return {'first_complete_attempt': True}
                self.step('release_gate', allow_complete)
                self.step('complete', lambda: self.maintenance_action('complete'))
            self.attempt.update(state='succeeded', ended_at=now())
            self.record['state'] = 'succeeded'
            self.record['outcome'] = 'rolled_back' if self.record.get('rollback') else 'deployed'
            self.save()
            durable_json(marker_path(r), {'deployment_id': self.id, 'record': str(self.path), 'state': 'succeeded'})
            return {'deployment_record': str(self.path), 'recovery_attempt': self.attempt['number'],
                    'software_acceptance': accepted, 'business_acceptance': 'not_performed',
                    'outcome': self.record['outcome'],
                    'process_actions_replayed': False, 'data_restored': False}
