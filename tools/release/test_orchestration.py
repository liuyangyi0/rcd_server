import json
from pathlib import Path
import platform
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from deployment import fingerprint
from orchestration import execute_once, stage, perform, staging_paths
from release import Release, sha


class OrchestrationTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        for name in ('source', 'runtime', 'releases', 'incoming', 'bin'):
            (self.root / name).mkdir()
        self.profile = {'host': '192.0.2.30', 'component': 'lower',
                        'source_dir': str(self.root / 'source'), 'runtime_dir': str(self.root / 'runtime'),
                        'release_dir': str(self.root / 'releases'), 'executable': str(self.root / 'bin/lower')}
        self.release = Release(self.profile, self.profile['host'], 'lower')
        self.incoming = self.root / 'incoming'
        self.destination = self.root / 'releases/staged'
        self.info = {'schema_version': 1, 'component': 'lower', 'version': '1.0.0-test',
                     'source_sha256': 'b' * 64, 'release_id': 'test', 'target': 'linux/' + platform.machine()}
        machine = 183 if platform.machine() == 'aarch64' else 62
        (self.incoming / 'binary').write_bytes(b'\x7fELF\x02\x01' + b'\0' * 12 + machine.to_bytes(2, 'little'))
        self.manifest = {'host': '192.0.2.46', 'component': 'lower', 'build_info': self.info,
                         'source': {'sha256': 'b' * 64}, 'binary_sha256': sha(self.incoming / 'binary')}
        (self.incoming / 'manifest.json').write_text(json.dumps(self.manifest))
        self.pin = sha(self.incoming / 'manifest.json')
        self.request = {'action': 'deploy', 'profile_sha256': fingerprint(self.profile),
                        'deployment_id': 'job', 'mode': 'maintenance', 'recover_operation_id': ''}

    def staged(self):
        with patch('orchestration.subprocess.run', return_value=subprocess.CompletedProcess([], 0, json.dumps(self.info))):
            return stage(self.release, self.incoming, self.destination, self.pin)

    def test_stage_is_idempotent_preserves_origin_and_never_copies_config(self):
        (self.incoming / 'config.toml').write_text('user-owned')
        self.assertTrue(self.staged()['changed'])
        self.assertFalse(self.staged()['changed'])
        self.assertEqual(sha(self.destination / 'build-manifest.json'), self.pin)
        self.assertEqual(json.loads((self.destination / 'build-manifest.json').read_text())['host'], '192.0.2.46')
        self.assertEqual(self.release.verify(self.destination)['host'], '192.0.2.30')
        self.assertFalse((self.destination / 'config.toml').exists())
        self.assertFalse(self.release.entry.exists())

    def test_stage_refuses_bad_manifest_or_binary(self):
        with self.assertRaisesRegex(RuntimeError, 'digest'):
            stage(self.release, self.incoming, self.destination, 'a' * 64)
        (self.incoming / 'binary').write_bytes(b'corrupted')
        with self.assertRaisesRegex(RuntimeError, 'digest'):
            self.staged()
        self.assertFalse(self.destination.exists())

    def test_stage_refuses_wrong_architecture(self):
        self.info['target'] = 'linux/incorrect'
        (self.incoming / 'manifest.json').write_text(json.dumps(self.manifest))
        self.pin = sha(self.incoming / 'manifest.json')
        with self.assertRaisesRegex(RuntimeError, 'architecture'):
            self.staged()

    def test_stage_loader_failure_does_not_publish(self):
        with patch('orchestration.subprocess.run', side_effect=OSError('missing loader')):
            with self.assertRaises(OSError):
                stage(self.release, self.incoming, self.destination, self.pin)
        self.assertFalse(self.destination.exists())
        self.assertEqual(list(self.destination.parent.iterdir()), [])

    def test_stage_rejects_identity_mismatch(self):
        with patch('orchestration.subprocess.run', return_value=subprocess.CompletedProcess([], 0, '{}')):
            with self.assertRaisesRegex(RuntimeError, 'identity'):
                stage(self.release, self.incoming, self.destination, self.pin)

    def test_registered_application_staging_never_executes_uploaded_binary(self):
        self.release.application=True
        self.release.p['component']='my-app'
        self.info['component']='my-app';self.manifest['component']='my-app'
        (self.incoming/'manifest.json').write_text(json.dumps(self.manifest))
        self.pin=sha(self.incoming/'manifest.json')
        with patch('orchestration.subprocess.run',side_effect=AssertionError('must not execute external package')):
            self.assertTrue(stage(self.release,self.incoming,self.destination,self.pin)['changed'])

    def test_staged_profile_binding_rejects_configuration_of_release_tool_changed(self):
        self.staged()
        changed = Release(dict(self.profile, opc_port=9999), self.profile['host'], 'lower')
        with self.assertRaisesRegex(RuntimeError, 'profile'):
            changed.verify(self.destination)

    def test_staged_original_and_target_manifest_tampering_rejected(self):
        self.staged()
        data = json.loads((self.destination / 'manifest.json').read_text())
        data['build_info']['version'] = 'forged'
        (self.destination / 'manifest.json').write_text(json.dumps(data))
        with self.assertRaisesRegex(RuntimeError, 'identity'):
            self.release.verify(self.destination)

    def test_stage_cannot_overwrite_existing_different_artifact(self):
        self.staged()
        self.info['version'] = '2.0.0-test'
        (self.incoming / 'manifest.json').write_text(json.dumps(self.manifest))
        self.pin = sha(self.incoming / 'manifest.json')
        with self.assertRaisesRegex(RuntimeError, 'immutable'):
            self.staged()

    def test_stage_rejects_unsafe_destinations(self):
        for dest in (self.root / 'runtime', self.root / 'releases', self.incoming):
            with self.assertRaises(ValueError):
                stage(self.release, self.incoming, dest, self.pin)

    def test_incoming_cannot_overlap_runtime_source_or_installed_binary(self):
        for incoming in (self.root / 'runtime', self.root / 'source', self.root / 'bin', self.root):
            with self.assertRaises(ValueError):
                staging_paths(self.release, incoming, self.destination)

    def test_success_replay_never_calls_control_again(self):
        calls = []
        callback = lambda: calls.append('once') or {'ok': True}
        self.assertTrue(execute_once(self.release, 'one', self.request, callback)['changed'])
        self.assertFalse(execute_once(self.release, 'one', self.request, callback)['changed'])
        self.assertEqual(calls, ['once'])

    def test_same_id_different_request_rejected(self):
        execute_once(self.release, 'one', self.request, lambda: {})
        with self.assertRaisesRegex(RuntimeError, 'conflict'):
            execute_once(self.release, 'one', dict(self.request, deployment_id='other'), lambda: {})

    def test_unknown_blocks_replay_and_new_deployment(self):
        with self.assertRaises(OSError):
            execute_once(self.release, 'one', self.request, lambda: (_ for _ in ()).throw(OSError()))
        for operation in ('one', 'two'):
            with self.assertRaises(RuntimeError):
                execute_once(self.release, operation, self.request, lambda: self.fail('replayed'))
        recovery = dict(self.request, action='finish-deployment')
        with self.assertRaisesRegex(RuntimeError, 'unfinished'):
            execute_once(self.release, 'wrong', dict(recovery, deployment_id='wrong'), lambda: {})
        result = execute_once(self.release, 'finish', recovery, lambda: {'finished': True})
        self.assertTrue(result['changed'])

    def test_manual_unknown_requires_explicit_original_operation_for_rollback(self):
        request = dict(self.request, mode='manual')
        with self.assertRaises(OSError):
            execute_once(self.release, 'manual', request, lambda: (_ for _ in ()).throw(OSError()))
        rollback = dict(request, action='rollback')
        with self.assertRaisesRegex(RuntimeError, 'unfinished'):
            execute_once(self.release, 'rb', rollback, lambda: {})
        self.assertTrue(execute_once(self.release, 'rb', dict(rollback, recover_operation_id='manual'), lambda: {})['changed'])

    def test_intent_is_durable_before_callback(self):
        def callback():
            saved = json.loads((self.root / 'releases/ansible-operations/one/operation.json').read_text())
            self.assertEqual(saved['state'], 'intent')
            return {}
        execute_once(self.release, 'one', self.request, callback)

    def test_journal_write_failure_prevents_control(self):
        with patch('orchestration.durable_json', side_effect=OSError('disk full')):
            with self.assertRaises(OSError):
                execute_once(self.release, 'one', self.request, lambda: self.fail('control after storage failure'))

    def test_dispatch_uses_existing_release_methods(self):
        with patch.object(self.release, 'deploy', return_value={'existing': True}) as deploy:
            self.assertEqual(perform(self.release, 'deploy', self.destination, 'manual', '', 'test', False), {'existing': True})
            deploy.assert_called_once_with(self.destination)
        with patch('orchestration.Deployment') as transaction:
            perform(self.release, 'deploy', self.destination, 'maintenance', 'job', 'test', False)
            transaction.return_value.deploy.assert_called_once_with(self.destination, 'test')

    def test_deploy_target_needs_no_source_checkout_or_compiler(self):
        (self.root / 'source').rmdir()
        self.release.entry.write_bytes(b'installed-image')
        with patch.object(self.release, 'local_host'), patch.object(self.release, 'pid', return_value=0):
            self.assertEqual(self.release.preflight()['binary_sha256'], sha(self.release.entry))
            with self.assertRaisesRegex(RuntimeError, 'build source'):
                self.release.build('a' * 40, True)


if __name__ == '__main__':
    unittest.main()
