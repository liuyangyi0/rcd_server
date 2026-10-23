"""Recovery fault injection only; actual Rust/agent coverage is opt-in Go interop."""
import json
import contextlib
import io
from unittest import TestCase
from unittest.mock import patch

from deployment import durable_json, marker_path
from recovery import FinishDeployment
import test_deployment as fixtures


class RecoveryTests(TestCase):
    setUp = fixtures.DeploymentTests.setUp
    run_deployment = fixtures.DeploymentTests.run_deployment
    mutations = fixtures.DeploymentTests.mutations

    def interrupted(self, point='start'):
        self.runner.fail = point
        with self.assertRaises((TimeoutError, RuntimeError)):
            self.run_deployment()
        self.runner.fail = ''

    def finish(self):
        return FinishDeployment(self.runner, 'job-001').finish('recovery-operator')

    def test_lost_start_reply_verified_without_second_process_action(self):
        self.interrupted()
        self.config.write_text('latest user configuration')
        result = self.finish()
        self.assertFalse(result['process_actions_replayed'])
        self.assertFalse(result['data_restored'])
        self.assertEqual(self.config.read_text(), 'latest user configuration')
        self.assertEqual([a for e, a in self.mutations() if e == '/api/v1/process/actions'], ['stop', 'start'])
        record = json.loads(self.deployment.path.read_text())
        self.assertEqual(record['stages'][-1]['state'], 'unknown')  # Original history not rewritten.
        self.assertEqual(record['recovery_attempts'][0]['state'], 'succeeded')
        self.assertEqual(record['state'], 'succeeded')
        with self.assertRaisesRegex(RuntimeError, 'already finished'):
            self.finish()

    def test_lost_complete_reply_never_calls_complete_twice(self):
        self.interrupted('complete')
        self.finish()
        self.assertEqual(sum(a == 'complete' for _, a in self.mutations()), 1)

    def test_failed_final_record_is_recoverable_without_replay(self):
        def fail_final(path, data):
            if path == self.deployment.path and data.get('state') == 'succeeded':
                raise OSError('disk full after successful completion')
            return durable_json(path, data)
        with patch('deployment.durable_json', side_effect=fail_final):
            with self.assertRaises(OSError):
                self.run_deployment()
        self.finish()
        self.assertEqual(sum(a == 'complete' for _, a in self.mutations()), 1)

    def test_early_failure_not_resumed(self):
        self.interrupted('stop')
        count = len(self.runner.calls)
        with self.assertRaisesRegex(RuntimeError, 'confirmed installation'):
            self.finish()
        self.assertEqual(count, len(self.runner.calls))

    def test_unknown_original_start_not_inferred_from_running_pid(self):
        self.interrupted()
        self.runner.operations['job-001.start']['state'] = 'unknown'
        with self.assertRaisesRegex(RuntimeError, 'original start not confirmed'):
            self.finish()
        self.assertEqual(sum(a == 'start' for _, a in self.mutations()), 1)
        self.assertFalse(any(a == 'complete' for _, a in self.mutations()))

    def test_no_complete_replay_even_if_agent_says_release_not_requested(self):
        self.interrupted('complete')
        self.runner.job_value['phase'] = 'ready'
        self.runner.job_value['lower'].update(active=True, admission_blocked=True, drained=True)
        with self.assertRaisesRegex(RuntimeError, 'prior complete intent'):
            self.finish()
        self.assertEqual(sum(a == 'complete' for _, a in self.mutations()), 1)

    def test_agent_release_requested_but_still_active_is_unknown(self):
        self.interrupted()
        self.runner.job_value['release_requested'] = True
        self.runner.job_value['phase'] = 'releasing'
        with self.assertRaisesRegex(RuntimeError, 'release still unconfirmed'):
            self.finish()
        self.assertFalse(any(a == 'complete' for _, a in self.mutations()))

    def test_wrong_generation_and_marker_refused(self):
        self.interrupted()
        self.runner.job_value['fencing_token'] = '2'
        with self.assertRaisesRegex(RuntimeError, 'generation mismatch'):
            self.finish()
        durable_json(marker_path(self.runner), {'deployment_id': 'someone-else', 'record': str(self.deployment.path)})
        with self.assertRaisesRegex(RuntimeError, 'another deployment'):
            self.finish()

    def test_corrupt_backup_or_replaced_binary_prevents_remote_mutation(self):
        self.interrupted()
        count = len(self.mutations())
        record = json.loads(self.deployment.path.read_text())
        from pathlib import Path
        backup = Path(record['stages'][3]['result']['path'])
        (backup / 'binary').write_bytes(b'corrupt')
        with self.assertRaisesRegex(RuntimeError, 'backup identity or binary damaged'):
            self.finish()
        self.runner.entry.write_bytes(b'another binary')
        with self.assertRaisesRegex(RuntimeError, 'installed binary changed'):
            self.finish()
        self.assertEqual(count, len(self.mutations()))

    def test_recovery_reply_loss_is_reconciled_without_duplicate_complete(self):
        self.interrupted()
        self.runner.fail = 'complete'
        with self.assertRaises(TimeoutError):
            self.finish()
        self.runner.fail = ''
        self.finish()
        self.assertEqual(sum(a == 'complete' for _, a in self.mutations()), 1)
        self.assertEqual(sum(a == 'start' for _, a in self.mutations()), 1)

    def test_recovery_intent_storage_failure_has_no_remote_side_effect(self):
        self.interrupted()
        count = len(self.mutations())
        with patch('deployment.durable_json', side_effect=OSError('read-only')):
            with self.assertRaises(OSError):
                self.finish()
        self.assertEqual(count, len(self.mutations()))

    def test_cli_finish_requires_current_target_and_window_confirmation(self):
        from release import main
        profile = self.artifact / 'profile.json'
        profile.write_text(json.dumps(self.profile))
        argv = ['release.py', 'finish-deployment', '--profile', str(profile), '--host', self.profile['host'],
                '--component', 'lower', '--deployment-id', 'job-001', '--initiator', 'operator']
        with patch('release.Release.local_host'), patch('recovery.FinishDeployment.finish') as finish:
            for flags in ([], ['--confirm-live-target', '192.0.2.1/lower'],
                          ['--confirm-live-target', '192.0.2.2/lower', '--confirm-no-active-commands']):
                with patch('sys.argv', argv + flags), contextlib.redirect_stderr(io.StringIO()):
                    with self.assertRaises(SystemExit) as error:
                        main()
                    self.assertEqual(error.exception.code, 2)
            finish.assert_not_called()

    def test_cli_resume_requires_current_target_and_window_confirmation(self):
        from release import main
        profile = self.artifact / 'profile.json'
        profile.write_text(json.dumps(self.profile))
        argv = ['release.py', 'resume-deployment', '--profile', str(profile), '--host', self.profile['host'],
                '--component', 'lower', '--deployment-id', 'job-001', '--initiator', 'operator']
        with patch('release.Release.local_host'), patch('resume.ResumeStoppedDeployment.resume') as resume:
            with patch('sys.argv', argv), contextlib.redirect_stderr(io.StringIO()):
                with self.assertRaises(SystemExit) as error: main()
                self.assertEqual(error.exception.code, 2)
            resume.assert_not_called()
