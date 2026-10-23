import contextlib
import io
import json
from pathlib import Path
import sqlite3
from unittest import TestCase
from unittest.mock import patch

from deployment import Deployment
from recovery import FinishDeployment
from resume import ResumeStoppedDeployment
from rollback_deployment import RollbackBeforeStart
import test_deployment as fixtures


class RollbackDeploymentTests(TestCase):
    setUp = fixtures.DeploymentTests.setUp
    run_deployment = fixtures.DeploymentTests.run_deployment
    mutations = fixtures.DeploymentTests.mutations

    def installed_without_start(self):
        step = Deployment.step
        def crash_before_start(instance, name, function, intent=None):
            if name == 'started':
                raise OSError('publisher died before start intent')
            return step(instance, name, function, intent)
        with patch.object(Deployment, 'step', crash_before_start):
            with self.assertRaises(OSError): self.run_deployment()
        self.assertEqual(self.runner.state, 'stopped')
        self.assertEqual(self.runner.entry.read_bytes(), b'new')

    def rollback(self):
        return RollbackBeforeStart(self.runner, 'job-001').rollback('operator')

    def finish(self):
        return RollbackBeforeStart(self.runner, 'job-001').finish('operator')

    def test_binary_only_rollback_preserves_user_configuration(self):
        self.installed_without_start()
        self.config.write_text('latest user edit')
        result = self.rollback()
        self.assertEqual(result['state'], 'awaiting_rollback_finish')
        self.assertTrue(self.runner.job_value['lower']['active'])
        self.assertEqual(self.runner.entry.read_bytes(), b'old')
        self.assertEqual(self.config.read_text(), 'latest user edit')
        result = self.finish()
        self.assertEqual(result['outcome'], 'rolled_back')
        self.assertFalse(result['data_restored'])
        self.assertEqual(sum(a == 'stop' for _, a in self.mutations()), 1)
        self.assertEqual(sum(a == 'start' for _, a in self.mutations()), 1)

    def test_new_start_intent_refuses_rollback_even_when_stopped(self):
        self.runner.fail = 'start'
        with self.assertRaises(TimeoutError): self.run_deployment()
        self.runner.state = 'stopped'
        with self.assertRaisesRegex(RuntimeError, 'before any upgrade start intent'): self.rollback()
        self.assertEqual(self.runner.entry.read_bytes(), b'new')

    def test_unknown_stop_refuses_rollback(self):
        self.installed_without_start()
        self.runner.operations['job-001.stop']['state'] = 'unknown'
        with self.assertRaisesRegex(RuntimeError, 'stop result unknown'): self.rollback()
        self.assertEqual(self.runner.entry.read_bytes(), b'new')

    def test_schema_drift_refuses_without_restoring_database(self):
        dbpath = self.runner.runtime / 'state.sqlite'
        with sqlite3.connect(dbpath) as db:
            db.execute('PRAGMA user_version=1')
            db.execute('CREATE TABLE data(value TEXT)')
        self.runner.databases.append(dbpath)
        self.profile['databases'] = [str(dbpath)]
        self.installed_without_start()
        with sqlite3.connect(dbpath) as db: db.execute('PRAGMA user_version=2')
        with self.assertRaisesRegex(RuntimeError, 'schema changed'): self.rollback()
        with sqlite3.connect(dbpath) as db: self.assertEqual(db.execute('PRAGMA user_version').fetchone()[0], 2)
        self.assertEqual(self.runner.entry.read_bytes(), b'new')

    def test_failed_preservation_has_no_binary_change_or_start(self):
        self.installed_without_start()
        with patch.object(self.runner, 'backup', side_effect=OSError('full')):
            with self.assertRaises(OSError): self.rollback()
        self.assertEqual(self.runner.entry.read_bytes(), b'new')
        self.assertEqual(sum(a == 'start' for _, a in self.mutations()), 0)

    def test_rollback_lost_start_reply_no_second_start(self):
        self.installed_without_start()
        self.runner.fail = 'start'
        with self.assertRaises(TimeoutError): self.rollback()
        self.runner.fail = ''
        with self.assertRaisesRegex(RuntimeError, 'rollback intent already exists'): self.rollback()
        with self.assertRaisesRegex(RuntimeError, 'rollback intent exists'):
            ResumeStoppedDeployment(self.runner, 'job-001').resume('operator')
        with self.assertRaisesRegex(RuntimeError, 'rollback intent exists'):
            FinishDeployment(self.runner, 'job-001').finish('operator')
        self.finish()
        self.assertEqual(sum(a == 'start' for _, a in self.mutations()), 1)

    def test_lost_rollback_complete_reply_no_second_complete(self):
        self.installed_without_start()
        self.rollback()
        self.runner.fail = 'complete'
        with self.assertRaises(TimeoutError): self.finish()
        self.runner.fail = ''
        self.finish()
        self.assertEqual(sum(a == 'complete' for _, a in self.mutations()), 1)

    def test_backup_corruption_or_wrong_old_binary_refuses(self):
        self.installed_without_start()
        record = json.loads(self.deployment.path.read_text())
        backup = Path(record['stages'][3]['result']['path'])
        (backup / 'binary').write_bytes(b'corrupt')
        with self.assertRaisesRegex(RuntimeError, 'backup identity or binary damaged'): self.rollback()
        self.assertEqual(self.runner.entry.read_bytes(), b'new')

    def test_cli_rollback_actions_require_current_confirmation(self):
        from release import main
        profile = self.artifact / 'profile.json'
        profile.write_text(json.dumps(self.profile))
        for action in ('rollback-deployment', 'finish-rollback'):
            argv = ['release.py', action, '--profile', str(profile), '--host', self.profile['host'],
                    '--component', 'lower', '--deployment-id', 'job-001', '--initiator', 'operator']
            with patch('release.Release.local_host'), patch('sys.argv', argv), contextlib.redirect_stderr(io.StringIO()):
                with self.assertRaises(SystemExit) as error: main()
                self.assertEqual(error.exception.code, 2)

    def test_self_consistent_backup_still_must_match_preupgrade_identity(self):
        from release import sha
        self.installed_without_start()
        record = json.loads(self.deployment.path.read_text())
        backup = Path(record['stages'][3]['result']['path'])
        (backup / 'binary').write_bytes(b'unrelated-but-hashed-binary')
        metadata = json.loads((backup / 'backup.json').read_text())
        metadata['binary_sha256'] = sha(backup / 'binary')
        (backup / 'backup.json').write_text(json.dumps(metadata))
        with self.assertRaisesRegex(RuntimeError, 'not the recorded pre-upgrade binary'): self.rollback()
        self.assertEqual(self.runner.entry.read_bytes(), b'new')
