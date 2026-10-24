"""Explicit stopped-lower continuation; no stop/restart replay or data restore."""
from pathlib import Path
from deployment import now, target_lock
from recovery import FinishDeployment, effective_stages


class ResumeStoppedDeployment(FinishDeployment):
    def checkpoint(self, stage, state, result=None, intent=None):
        entry = {'stage': stage, 'state': state, 'recorded_at': now(),
                 'result': result or {}, 'intent': intent or {}}
        self.record.setdefault('resume_checkpoints', []).append(entry)
        effective_stages(self.record)  # Validate before publishing the checkpoint.
        self.save()
        self.stages = effective_stages(self.record)

    def stopped(self):
        state = self.process_snapshot()
        if (state.get('state') != 'stopped' or state.get('desired_state') != 'stopped' or
                int(state.get('pid') or 0) != 0 or state.get('pending_request_id')):
            raise RuntimeError('resume requires stopped lower without pending operations')
        operation = self.release.json_request('/api/v1/process/operations/' + self.id + '.stop')
        if (operation.get('state') != 'succeeded' or operation.get('action') != 'stop' or
                operation.get('instance_id') != self.record['instance_id'] or
                operation.get('request_id') != self.id + '.stop'):
            raise RuntimeError('original stop result unknown; no stop replay')
        evidence = self.job(self.maintenance())
        if evidence['drained'] is not True or evidence['pending_admissions'] != 0 or evidence['pending_port_operations'] != 0:
            raise RuntimeError('durable cached fence does not confirm drained stop')
        # OPC cannot be read while stopped. This evidence must remain labelled
        # cached; start only permits the already-confirmed agent generation.
        return {'request_id': self.id + '.stop', 'state': 'succeeded', 'pid': 0,
                'fence_evidence': 'agent_cached_confirmed', 'fencing_token': self.token}

    def resume(self, initiator):
        from release import sha, atomic_copy
        r = self.release
        r.local_host()
        if not initiator.strip() or len(initiator) > 128:
            raise ValueError('recovery initiator required')
        with target_lock(r):
            self.load_record()
            if len(self.stages) > 5 or any(s.get('stage') == 'first_start' for a in self.record.get('recovery_attempts', []) for s in a.get('stages', [])):
                raise RuntimeError('a start intent already exists; inspect/finish, never start again')
            if self.stages[2].get('intent', {}).get('request_id') != self.id + '.stop':
                raise RuntimeError('original stop request ID missing')
            if not self.record.get('previous_binary_sha256'):
                raise RuntimeError('older journal lacks pre-install binary identity; manual recovery required')
            attempts = self.record.setdefault('recovery_attempts', [])
            if len(attempts) >= 100:
                raise RuntimeError('recovery attempt limit reached')
            self.attempt = {'number': len(attempts) + 1, 'kind': 'resume_stopped', 'initiator': initiator,
                            'started_at': now(), 'state': 'running', 'stages': []}
            attempts.append(self.attempt)
            self.save()
            self.step('artifact_verified', lambda: {'path': str(self.verify_artifact())})
            proof = self.step('stopped_verified', self.stopped)
            if self.stages[2]['state'] != 'confirmed':
                self.checkpoint('stopped', 'confirmed', proof, {'request_id': self.id + '.stop'})

            if len(self.stages) < 4 or self.stages[3]['state'] != 'confirmed':
                def backup():
                    self.stopped()
                    if r.entry.is_symlink() or sha(r.entry) != self.record['previous_binary_sha256']:
                        raise RuntimeError('original binary changed before backup')
                    return {'path': str(r.backup(Path(self.record['artifact'])))}
                # Never replace an incomplete backup directory; each retry gets
                # a new one and leaves earlier forensic files untouched.
                saved = self.step('backup_created', backup)
                self.checkpoint('backed_up', 'confirmed', saved)
            self.step('backup_verified', lambda: self.verify_backup(Path(self.stages[3]['result']['path'])))

            def install():
                self.stopped()
                artifact = self.verify_artifact()
                current = sha(r.entry)
                if r.entry.is_symlink() or current not in (self.record['previous_binary_sha256'], self.record['binary_sha256']):
                    raise RuntimeError('entry is neither original nor target binary')
                if current != self.record['binary_sha256']:
                    atomic_copy(artifact / 'binary', r.entry, executable=True)
                if sha(r.entry) != self.record['binary_sha256']:
                    raise RuntimeError('installed binary verification failed')
                return {'binary_sha256': self.record['binary_sha256']}

            if len(self.stages) < 5 or self.stages[4]['state'] != 'confirmed':
                installed = self.step('installed_verified', install)
                self.checkpoint('installed', 'confirmed', installed)
            self.step('ready_to_start', self.verify_files)
            self.step('stopped_rechecked', self.stopped)
            # Persist the canonical start intent BEFORE any POST. Even a crash
            # immediately after this save forbids resubmission on resume.
            self.checkpoint('started', 'intent', intent={'request_id': self.id + '.start'})
            started = self.step('first_start', lambda: self.process_action('start'))
            self.checkpoint('started', 'confirmed', started, {'request_id': self.id + '.start'})
            self.attempt.update(state='awaiting_finish', ended_at=now())
            self.record['state'] = 'awaiting_finish'
            self.save()
            return {'deployment_record': str(self.path), 'state': 'awaiting_finish',
                    'next_action': 'finish-deployment', 'maintenance_released': False,
                    'data_restored': False}
