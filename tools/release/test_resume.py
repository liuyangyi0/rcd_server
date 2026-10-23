import json
from unittest import TestCase
from unittest.mock import patch

from recovery import FinishDeployment
from resume import ResumeStoppedDeployment
import test_deployment as fixtures


class ResumeTests(TestCase):
    setUp = fixtures.DeploymentTests.setUp
    run_deployment = fixtures.DeploymentTests.run_deployment
    mutations = fixtures.DeploymentTests.mutations

    def stopped_reply_lost(self):
        self.runner.fail = 'stop'
        with self.assertRaises(TimeoutError):
            self.run_deployment()
        self.runner.fail = ''

    def resume(self):
        return ResumeStoppedDeployment(self.runner, 'job-001').resume('operator')

    def finish(self):
        return FinishDeployment(self.runner, 'job-001').finish('operator')

    def test_stopped_reply_recovered_then_install_start_finish(self):
        self.stopped_reply_lost()
        self.config.write_text('new user config while stopped')
        result = self.resume()
        self.assertEqual(result['state'], 'awaiting_finish')
        self.assertTrue(self.runner.job_value['lower']['active'])
        self.finish()
        self.assertEqual(self.config.read_text(), 'new user config while stopped')
        self.assertEqual([a for e, a in self.mutations() if e == '/api/v1/process/actions'], ['stop', 'start'])
        record = json.loads(self.deployment.path.read_text())
        self.assertEqual(record['stages'][-1]['state'], 'unknown')
        self.assertEqual(record['state'], 'succeeded')

    def test_backup_failure_retry_preserves_current_data(self):
        with patch.object(self.runner, 'backup', side_effect=OSError('full')):
            with self.assertRaises(OSError): self.run_deployment()
        self.resume()
        self.finish()
        self.assertEqual(self.runner.entry.read_bytes(), b'new')

    def test_install_confirmation_loss_detects_existing_binary_without_recopy(self):
        from deployment import durable_json
        def fail_confirmation(path, record):
            stages = record.get('stages', [])
            if stages and stages[-1]['stage'] == 'installed' and stages[-1]['state'] == 'confirmed':
                raise OSError('lost confirmation')
            return durable_json(path, record)
        with patch('deployment.durable_json', side_effect=fail_confirmation):
            with self.assertRaises(OSError): self.run_deployment()
        self.assertEqual(self.runner.entry.read_bytes(), b'new')
        with patch('release.atomic_copy', side_effect=AssertionError('must not recopy')):
            self.resume()
        self.finish()

    def test_unknown_stop_not_replayed(self):
        self.stopped_reply_lost()
        self.runner.operations['job-001.stop']['state'] = 'unknown'
        with self.assertRaisesRegex(RuntimeError, 'stop result unknown'): self.resume()
        self.assertEqual(sum(a == 'stop' for _, a in self.mutations()), 1)
        self.assertEqual(self.runner.entry.read_bytes(), b'old')

    def test_running_or_unconfirmed_gate_blocks_install(self):
        self.stopped_reply_lost()
        self.runner.state = 'running'
        with self.assertRaisesRegex(RuntimeError, 'requires stopped'): self.resume()
        self.runner.state = 'stopped'
        self.runner.job_value['lower_confirmed'] = False
        with self.assertRaisesRegex(RuntimeError, 'gate not confirmed'): self.resume()
        self.assertEqual(self.runner.entry.read_bytes(), b'old')

    def test_changed_binary_before_backup_refused(self):
        self.stopped_reply_lost()
        self.runner.entry.write_bytes(b'not the original')
        with self.assertRaisesRegex(RuntimeError, 'original binary changed'): self.resume()

    def test_start_reply_lost_during_resume_only_finish_allowed(self):
        self.stopped_reply_lost()
        self.runner.fail = 'start'
        with self.assertRaises(TimeoutError): self.resume()
        self.runner.fail = ''
        with self.assertRaisesRegex(RuntimeError, 'start intent already exists'): self.resume()
        self.finish()
        self.assertEqual(sum(a == 'start' for _, a in self.mutations()), 1)

    def test_crash_before_start_post_still_forbids_replay(self):
        self.stopped_reply_lost()
        with patch.object(ResumeStoppedDeployment, 'process_action', side_effect=OSError('crash before POST')):
            with self.assertRaises(OSError): self.resume()
        with self.assertRaisesRegex(RuntimeError, 'start intent already exists'): self.resume()
        self.assertEqual(sum(a == 'start' for _, a in self.mutations()), 0)
        self.assertTrue(self.runner.job_value['lower']['active'])

    def test_older_journal_without_binary_identity_refused(self):
        self.stopped_reply_lost()
        data = json.loads(self.deployment.path.read_text())
        del data['previous_binary_sha256']
        self.deployment.path.write_text(json.dumps(data))
        with self.assertRaisesRegex(RuntimeError, 'older journal'): self.resume()
