"""Binary-only abort of a stopped upgrade that has NEVER attempted new start.

Not a post-migration downgrade. No database/configuration/token restoration.
"""
import json
import sqlite3
from contextlib import closing
from pathlib import Path

from deployment import now, target_lock
from recovery import FinishDeployment
from resume import ResumeStoppedDeployment


class RollbackBeforeStart(FinishDeployment):
    allow_rollback = True

    def eligible(self):
        if (not 4 <= len(self.stages) <= 5 or self.stages[3]['state'] != 'confirmed' or
                self.stages[2].get('intent', {}).get('request_id') != self.id + '.stop' or
                not self.record.get('previous_binary_sha256') or
                any(s.get('stage') == 'first_start' for a in self.record.get('recovery_attempts', []) for s in a.get('stages', []))):
            raise RuntimeError('rollback only supports a confirmed backup before any upgrade start intent')

    def backup_record(self):
        backup = Path(self.stages[3]['result']['path'])
        self.verify_backup(backup)
        record = json.loads((backup / 'backup.json').read_text())
        if record['binary_sha256'] != self.record['previous_binary_sha256'] or (backup / 'binary').is_symlink():
            raise RuntimeError('backup binary is not the recorded pre-upgrade binary')
        return backup, record

    def compatible_state(self):
        backup, record = self.backup_record()
        self.release.validate_state_inventory()
        # The new program has never been started by this workflow. Nevertheless,
        # reject observed schema drift rather than trying to restore old data.
        schemas = {}
        for item in record['files']:
            if not item['database']:
                continue
            path = Path(item['target'])
            if not path.is_file() or path.is_symlink():
                raise RuntimeError('current state database missing or redirected')
            with closing(sqlite3.connect(path.as_uri() + '?mode=ro', uri=True)) as db:
                if db.execute('PRAGMA quick_check').fetchone()[0] != 'ok':
                    raise RuntimeError('current database integrity failure')
                schema = db.execute('PRAGMA user_version').fetchone()[0]
            if schema != item['schema_version']:
                raise RuntimeError('database schema changed; binary-only rollback refused')
            schemas[item['target']] = schema
        return {'backup': str(backup), 'current_schemas': schemas, 'data_restored': False}

    def stopped(self):
        return ResumeStoppedDeployment.stopped(self)

    def check_entry(self):
        from release import sha
        entry = self.release.entry
        digest = sha(entry)
        if entry.is_symlink() or digest not in (self.record['previous_binary_sha256'], self.record['binary_sha256']):
            raise RuntimeError('entry is neither original nor upgrade binary')
        return {'binary_sha256': digest}

    def restore_binary(self):
        from release import sha, atomic_copy
        self.stopped()
        self.compatible_state()
        backup, _ = self.backup_record()
        self.check_entry()
        if sha(self.release.entry) != self.record['previous_binary_sha256']:
            atomic_copy(backup / 'binary', self.release.entry, executable=True)
        if sha(self.release.entry) != self.record['previous_binary_sha256']:
            raise RuntimeError('rollback binary hash mismatch')
        return {'binary_sha256': self.record['previous_binary_sha256']}

    def start_rollback(self):
        self.step('old_binary_restored', self.restore_binary)
        self.step('rollback_files_verified', self.verify_files)
        self.step('stopped_rechecked', self.stopped)
        self.record['rollback']['start_intent'] = True
        self.record['rollback']['request_id'] = self.id + '.rollback-start'
        self.save()  # Even a crash before POST now forbids another start.
        self.step('rollback_first_start', lambda: self.process_action('start', self.id + '.rollback-start'))
        self.attempt.update(state='awaiting_finish', ended_at=now())
        self.record['state'] = 'awaiting_rollback_finish'
        self.save()
        return {'state': 'awaiting_rollback_finish', 'next_action': 'finish-rollback',
                'deployment_record': str(self.path), 'data_restored': False, 'maintenance_released': False}

    def new_attempt(self, kind, initiator):
        attempts = self.record.setdefault('recovery_attempts', [])
        if len(attempts) >= 100:
            raise RuntimeError('recovery attempt limit reached')
        self.attempt = {'number': len(attempts) + 1, 'kind': kind, 'initiator': initiator,
                        'started_at': now(), 'state': 'running', 'stages': []}
        attempts.append(self.attempt)
        self.save()

    def resume_rollback(self, initiator):
        """Continue a recorded rollback only before ANY rollback start intent.

        The original history and preservation backup remain immutable. Each
        continuation preserves current user data again, without restoring it.
        """
        r = self.release
        r.local_host()
        if not initiator.strip() or len(initiator) > 128:
            raise ValueError('rollback initiator required')
        with target_lock(r):
            self.load_record()
            self.eligible()
            rollback = self.record.get('rollback') or {}
            if (rollback.get('binary_sha256') != self.record['previous_binary_sha256'] or
                    rollback.get('start_intent') is not False or rollback.get('request_id') or
                    any(s.get('stage') == 'rollback_first_start' for a in self.record.get('recovery_attempts', []) for s in a.get('stages', []))):
                raise RuntimeError('resume-rollback requires recorded rollback without any start intent; inspect/finish-rollback instead')
            self.new_attempt('resume_rollback_before_start', initiator)
            self.step('stopped_verified', self.stopped)
            self.step('rollback_compatibility', self.compatible_state)
            self.step('rollback_entry_verified', self.check_entry)
            self.step('original_preservation_verified', lambda: self.verify_backup(Path(rollback['preserved_current_backup'])))
            # User configuration may have changed since the interruption. Never
            # overwrite the previous copy, and never resume from a partial one.
            preserved = self.step('resume_rollback_backup', lambda: {'path': str(r.backup(Path(self.record['artifact'])))})
            self.step('resume_preservation_verified', lambda: self.verify_backup(Path(preserved['path'])))
            return self.start_rollback()

    def rollback(self, initiator):
        r = self.release
        r.local_host()
        if not initiator.strip() or len(initiator) > 128:
            raise ValueError('rollback initiator required')
        with target_lock(r):
            self.load_record()
            self.eligible()
            if self.record.get('rollback'):
                raise RuntimeError('rollback intent already exists; inspect/finish-rollback, do not replay')
            self.new_attempt('rollback_before_start', initiator)
            self.step('stopped_verified', self.stopped)
            self.step('rollback_compatibility', self.compatible_state)
            self.step('rollback_entry_verified', self.check_entry)
            # Preserve CURRENT user state before changing the entry. This copy
            # is never restored automatically, including on subsequent failure.
            preserved = self.step('pre_rollback_backup', lambda: {'path': str(r.backup(Path(self.record['artifact'])))})
            self.record['rollback'] = {'binary_sha256': self.record['previous_binary_sha256'],
                                       'preserved_current_backup': preserved['path'], 'initiator': initiator,
                                       'started_at': now(), 'start_intent': False}
            self.save()  # Permanently prevents resuming the upgrade on this ID.
            return self.start_rollback()

    def load(self):
        self.load_record()
        self.eligible()
        rollback = self.record.get('rollback') or {}
        if (rollback.get('binary_sha256') != self.record['previous_binary_sha256'] or
                rollback.get('start_intent') is not True or rollback.get('request_id') != self.id + '.rollback-start'):
            raise RuntimeError('rollback start is not recorded; manual recovery required')
        self.complete_attempted = any(s.get('stage') == 'complete' for a in self.record.get('recovery_attempts', []) for s in a.get('stages', []))

    def verify_files(self):
        from release import sha
        self.backup_record()
        # Verify the separate pre-rollback copy as well as the original backup.
        self.verify_backup(Path(self.record['rollback']['preserved_current_backup']))
        # Confirmed continuation copies also remain recovery evidence. Partial
        # failed copies are retained for inspection, never used for recovery.
        for attempt in self.record.get('recovery_attempts', []):
            for step in attempt.get('stages', []):
                if step.get('stage') == 'resume_preservation_verified' and step.get('state') == 'confirmed':
                    self.verify_backup(Path(step['result']['backup_verified']))
        if self.release.entry.is_symlink() or sha(self.release.entry) != self.acceptance_hash():
            raise RuntimeError('rollback entry does not match old binary')
        return {'binary_sha256': self.acceptance_hash(), 'backups_verified': True}

    def verify_started(self):
        return self.verify_start_request(self.id + '.rollback-start')

    def acceptance_hash(self):
        return self.record['rollback']['binary_sha256']
