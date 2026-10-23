"""Explicit rollback continuation faults. No hardware or live services."""
import contextlib
import copy
import io
import json
from pathlib import Path
from unittest import TestCase
from unittest.mock import patch

from deployment import Deployment
from recovery import FinishDeployment
from rollback_deployment import RollbackBeforeStart
import test_rollback_deployment as fixtures


class ResumeRollbackTests(TestCase):
    setUp = fixtures.RollbackDeploymentTests.setUp
    run_deployment = fixtures.RollbackDeploymentTests.run_deployment
    mutations = fixtures.RollbackDeploymentTests.mutations
    installed_without_start = fixtures.RollbackDeploymentTests.installed_without_start
    rollback = fixtures.RollbackDeploymentTests.rollback
    finish = fixtures.RollbackDeploymentTests.finish

    def interrupted(self, after_restore=False):
        self.installed_without_start()
        original = FinishDeployment.step
        def stop(instance, name, function):
            if name == ('rollback_files_verified' if after_restore else 'old_binary_restored'):
                raise OSError('publisher interrupted before rollback start intent')
            return original(instance, name, function)
        with patch.object(FinishDeployment, 'step', stop):
            with self.assertRaises(OSError): self.rollback()
        self.assertFalse(self.record()['rollback']['start_intent'])
        self.assertEqual(self.runner.state, 'stopped')

    def record(self):
        return json.loads(self.deployment.path.read_text())

    def resume(self):
        return RollbackBeforeStart(self.runner, 'job-001').resume_rollback('resume-operator')

    def test_resume_before_restore_preserves_new_user_edits_and_history(self):
        self.interrupted()
        before = self.record()
        self.config.write_text('user changed after interrupted rollback')
        result = self.resume()
        self.assertEqual(result['state'], 'awaiting_rollback_finish')
        self.assertFalse(result['maintenance_released'])
        self.assertEqual(self.runner.entry.read_bytes(), b'old')
        self.assertEqual(self.config.read_text(), 'user changed after interrupted rollback')
        after = self.record()
        self.assertEqual(after['stages'], before['stages'])
        self.assertEqual(after['recovery_attempts'][:-1], before['recovery_attempts'])
        self.assertEqual(after['rollback']['preserved_current_backup'], before['rollback']['preserved_current_backup'])
        steps = after['recovery_attempts'][-1]['stages']
        saved = Path(next(s['result']['path'] for s in steps if s['stage'] == 'resume_rollback_backup'))
        self.assertNotEqual(str(saved), before['rollback']['preserved_current_backup'])
        self.assertEqual((saved / 'file-0').read_text(), self.config.read_text())
        self.assertEqual(self.finish()['outcome'], 'rolled_back')
        self.assertEqual([a for _, a in self.mutations()].count('stop'), 1)
        self.assertEqual([a for _, a in self.mutations()].count('start'), 1)

    def test_already_restored_binary_not_copied_again(self):
        self.interrupted(after_restore=True)
        with patch('release.atomic_copy', side_effect=AssertionError('must not recopy old binary')):
            self.resume()
        self.assertEqual(self.runner.entry.read_bytes(), b'old')

    def test_any_start_intent_refuses_even_if_post_never_happened(self):
        self.interrupted()
        original = FinishDeployment.step
        def die(instance, name, function):
            if name == 'rollback_first_start': raise OSError('crash after intent before POST')
            return original(instance, name, function)
        with patch.object(FinishDeployment, 'step', die):
            with self.assertRaises(OSError): self.resume()
        self.assertTrue(self.record()['rollback']['start_intent'])
        with self.assertRaisesRegex(RuntimeError, 'without any start intent'): self.resume()
        self.assertNotIn('start', [a for _, a in self.mutations()])

    def test_lost_start_reply_only_finish_no_start_replay(self):
        self.interrupted()
        self.runner.fail = 'start'
        with self.assertRaises(TimeoutError): self.resume()
        self.runner.fail = ''
        with self.assertRaisesRegex(RuntimeError, 'without any start intent'): self.resume()
        self.assertEqual(self.finish()['outcome'], 'rolled_back')
        self.assertEqual([a for _, a in self.mutations()].count('start'), 1)

    def test_start_intent_save_failure_never_posts(self):
        self.interrupted()
        original = RollbackBeforeStart.save
        def fail(instance):
            if instance.record.get('rollback', {}).get('start_intent'):
                raise OSError('disk full before start intent persisted')
            return original(instance)
        with patch.object(RollbackBeforeStart, 'save', fail):
            with self.assertRaises(OSError): self.resume()
        self.assertNotIn('start', [a for _, a in self.mutations()])
        self.assertFalse(self.record()['rollback']['start_intent'])
        self.resume()
        self.assertEqual([a for _, a in self.mutations()].count('start'), 1)

    def test_failed_fresh_backup_does_not_restore_or_start(self):
        self.interrupted()
        old = self.record()['rollback']['preserved_current_backup']
        with patch.object(self.runner, 'backup', side_effect=OSError('disk full')):
            with self.assertRaises(OSError): self.resume()
        self.assertEqual(self.runner.entry.read_bytes(), b'new')
        self.assertEqual(self.record()['rollback']['preserved_current_backup'], old)
        self.assertNotIn('start', [a for _, a in self.mutations()])
        self.resume()

    def test_damaged_original_preservation_blocks_before_new_backup(self):
        self.interrupted()
        saved = Path(self.record()['rollback']['preserved_current_backup'])
        (saved / 'binary').write_bytes(b'broken')
        with patch.object(self.runner, 'backup', side_effect=AssertionError('should not backup')):
            with self.assertRaisesRegex(RuntimeError, 'backup identity or binary damaged'): self.resume()
        self.assertEqual(self.runner.entry.read_bytes(), b'new')

    def test_confirmed_resume_backup_damage_blocks_finish(self):
        self.interrupted()
        self.resume()
        steps = self.record()['recovery_attempts'][-1]['stages']
        saved = Path(next(s['result']['path'] for s in steps if s['stage'] == 'resume_rollback_backup'))
        (saved / 'file-0').write_text('corrupted preservation')
        with self.assertRaisesRegex(RuntimeError, 'backup file damaged'): self.finish()
        self.assertNotIn('complete', [a for _, a in self.mutations()])
        self.assertTrue(self.runner.job_value['lower']['active'])

    def test_schema_drift_since_interruption_keeps_current_database(self):
        import sqlite3
        path = self.runner.runtime / 'state.sqlite'
        with sqlite3.connect(path) as db: db.execute('PRAGMA user_version=1')
        self.runner.databases.append(path)
        self.profile['databases'] = [str(path)]
        self.interrupted()
        with sqlite3.connect(path) as db: db.execute('PRAGMA user_version=2')
        with self.assertRaisesRegex(RuntimeError, 'schema changed'): self.resume()
        with sqlite3.connect(path) as db: self.assertEqual(db.execute('PRAGMA user_version').fetchone()[0], 2)
        self.assertEqual(self.runner.entry.read_bytes(), b'new')
        self.assertNotIn('start', [a for _, a in self.mutations()])

    def test_drift_and_unconfirmed_stop_refuse_without_mutation(self):
        for fault in ('entry', 'stop', 'token', 'profile', 'running'):
            with self.subTest(fault=fault):
                self.setUp(); self.interrupted()
                if fault == 'entry': self.runner.entry.write_bytes(b'unknown binary')
                elif fault == 'stop': self.runner.operations['job-001.stop']['state'] = 'unknown'
                elif fault == 'token': self.runner.job_value['fencing_token'] = '99'
                elif fault == 'profile': self.runner.p['gateway_instance_id'] = 'another'
                else: self.runner.state = 'running'
                before = copy.deepcopy(self.mutations())
                with self.assertRaises(RuntimeError): self.resume()
                self.assertEqual(self.mutations(), before)

    def test_missing_rollback_and_contradictory_start_history_refuse(self):
        self.installed_without_start()
        with self.assertRaisesRegex(RuntimeError, 'recorded rollback'): self.resume()
        # Register a rollback, then inject an inconsistent journal. The explicit
        # false flag alone cannot override a persisted start-stage intent.
        with patch.object(RollbackBeforeStart, 'start_rollback', side_effect=OSError('interrupted')):
            with self.assertRaises(OSError): self.rollback()
        record = self.record()
        record['recovery_attempts'][-1]['stages'].append({'stage': 'rollback_first_start', 'state': 'intent'})
        self.deployment.path.write_text(json.dumps(record))
        with self.assertRaisesRegex(RuntimeError, 'without any start intent'): self.resume()

    def test_attempt_limit_and_changed_artifact_refuse(self):
        self.interrupted()
        original = self.record()
        record = copy.deepcopy(original)
        record['recovery_attempts'] = [{} for _ in range(100)]
        self.deployment.path.write_text(json.dumps(record))
        with self.assertRaisesRegex(RuntimeError, 'attempt limit'): self.resume()
        self.deployment.path.write_text(json.dumps(original))
        (self.artifact / 'binary').write_bytes(b'changed')
        with self.assertRaises(RuntimeError): self.resume()
        self.assertNotIn('start', [a for _, a in self.mutations()])

    def test_cli_requires_both_current_confirmations(self):
        from release import main
        profile = self.artifact / 'profile.json'
        profile.write_text(json.dumps(self.profile))
        base = ['release.py', 'resume-rollback', '--profile', str(profile), '--host', self.profile['host'],
                '--component', 'lower', '--deployment-id', 'job-001', '--initiator', 'operator']
        for extra in ([], ['--confirm-no-active-commands'], ['--confirm-live-target', self.profile['host'] + '/lower']):
            with patch('release.Release.local_host'), patch('sys.argv', base + extra), contextlib.redirect_stderr(io.StringIO()):
                with self.assertRaises(SystemExit) as error: main()
                self.assertEqual(error.exception.code, 2)

    def test_inspection_remains_read_only_and_exposes_resume_history(self):
        self.interrupted()
        self.resume()
        before = self.mutations()
        result = Deployment(self.runner, 'job-001').inspect()
        self.assertFalse(result['automatic_resume'])
        self.assertEqual(result['recovery_attempts'][-1]['kind'], 'resume_rollback_before_start')
        self.assertEqual(self.mutations(), before)
