"""Isolated fault tests; no services, OPC ports, or serial devices are touched."""
import copy
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from deployment import Deployment, marker_path, target_lock
from release import Release, sha


class AgentFixture(Release):
    def local_host(self):
        pass

    def preflight(self):
        return {'isolated': True}

    def accept(self, expected):
        if self.fail == 'accept':
            raise RuntimeError('fixture acceptance failed')
        assert sha(self.entry) == expected
        return {'binary_sha256': expected, 'isolated': True}

    def json_request(self, endpoint, payload=None):
        self.calls.append((endpoint, copy.deepcopy(payload)))
        if endpoint == '/api/v1/process':
            return {'instance_id': 'station', 'revision': 'r1', 'executable': str(self.entry),
                    'work_dir': str(self.runtime), 'state': self.state, 'desired_state': self.state,
                    'pid': 0 if self.state == 'stopped' else 100}
        if endpoint == '/api/v1/maintenance':
            return self.snapshot()
        if endpoint == '/api/v1/maintenance/actions':
            action = payload['action']
            if action == 'enter':
                self.job_value = {'deployment_id': payload['deployment_id'], 'instance_id': 'station',
                                  'gateway_instance_id': 'gateway', 'endpoint': 'opc.tcp://127.0.0.1:4999',
                                  'target_version': payload['target_version'], 'fencing_token': '1',
                                  'phase': 'ready', 'lower_confirmed': True, 'release_requested': False,
                                  'lower': {'gateway_instance_id': 'gateway', 'boot_id': 'boot-1',
                                            'deployment_id': payload['deployment_id'], 'fencing_token': '1',
                                            'active': True, 'admission_blocked': True, 'durable': True,
                                            'drained': True, 'pending_admissions': 0, 'pending_port_operations': 0}}
            elif action == 'complete':
                self.job_value['phase'] = 'completed'
                self.job_value['lower'].update(active=False, admission_blocked=False, drained=False)
            elif action == 'reconcile' and self.fail == 'new-boot-wrong-owner' and self.started:
                self.job_value['lower']['fencing_token'] = '2'
            if self.fail == action:
                raise TimeoutError('reply lost; password=secret-must-not-be-logged')
            return self.snapshot()
        if endpoint == '/api/v1/process/actions':
            action = payload['action']
            self.state = 'stopped' if action == 'stop' else 'running'
            self.started = self.started or action == 'start'
            self.operations[payload['request_id']] = dict(payload, state='succeeded')
            if self.fail == action:
                raise TimeoutError('reply lost')
            return self.operations[payload['request_id']]
        if endpoint.startswith('/api/v1/process/operations/'):
            return self.operations[endpoint.rsplit('/', 1)[1]]
        raise AssertionError(endpoint)

    def snapshot(self):
        return copy.deepcopy({'supported': self.supported, 'revision': 'r1', 'job': self.job_value,
                              'gateway_instance_id': self.gateway, 'endpoint': 'opc.tcp://127.0.0.1:4999'})


class DeploymentTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        base = Path(self.temp.name)
        runtime, source, builds = (base / p for p in ('runtime', 'source', 'releases'))
        for directory in (runtime, source, builds):
            directory.mkdir()
        entry = runtime / 'lower'
        entry.write_bytes(b'old')
        self.config = runtime / 'config.toml'
        self.config.write_text('user current config')
        self.profile = {'host': '192.0.2.1', 'component': 'lower', 'runtime_dir': str(runtime),
                        'source_dir': str(source), 'release_dir': str(builds), 'executable': str(entry),
                        'config_files': [str(self.config)], 'gateway_instance_id': 'gateway',
                        'maintenance_endpoint': 'opc.tcp://127.0.0.1:4999',
                        'control': {'kind': 'process-agent', 'instance_id': 'station', 'url': 'http://127.0.0.1:8999'}}
        self.runner = AgentFixture(self.profile, self.profile['host'], 'lower')
        self.runner.calls, self.runner.operations = [], {}
        self.runner.state, self.runner.job_value = 'running', None
        self.runner.fail, self.runner.started = '', False
        self.runner.supported, self.runner.gateway = True, 'gateway'
        self.artifact = builds / 'build-test'
        self.artifact.mkdir()
        (self.artifact / 'binary').write_bytes(b'new')
        self.manifest = {'host': self.profile['host'], 'component': 'lower',
                         'binary_sha256': sha(self.artifact / 'binary'), 'build_info': {'version': '1.2.3'}}
        (self.artifact / 'manifest.json').write_text(json.dumps(self.manifest))
        self.deployment = Deployment(self.runner, 'job-001')

    def run_deployment(self):
        return self.deployment.deploy(self.artifact, 'test-operator')

    def mutations(self):
        return [(endpoint, payload['action']) for endpoint, payload in self.runner.calls if payload is not None]

    def test_success_journal_order_and_config_preserved(self):
        result = self.run_deployment()
        record = json.loads(self.deployment.path.read_text())
        self.assertEqual(record['state'], 'succeeded')
        self.assertEqual([s['stage'] for s in record['stages']], [
            'maintenance_enter', 'drained', 'stopped', 'backed_up', 'installed', 'started',
            'software_acceptance', 'lower_verified', 'maintenance_completed'])
        self.assertTrue(all(s['state'] == 'confirmed' for s in record['stages']))
        self.assertEqual(self.config.read_text(), 'user current config')
        self.assertEqual(self.runner.entry.read_bytes(), b'new')
        self.assertEqual(Path(result['recovery_backup'], 'binary').read_bytes(), b'old')
        self.assertEqual(result['business_acceptance'], 'not_performed')
        self.assertEqual(self.runner.job_value['phase'], 'completed')

    def test_lost_replies_never_resubmit_or_fallback(self):
        for action in ('enter', 'stop', 'start', 'complete'):
            with self.subTest(action=action):
                # Each case uses an entirely independent target/journal.
                self.setUp()
                self.runner.fail = action
                with self.assertRaises(TimeoutError):
                    self.run_deployment()
                calls = len(self.runner.calls)
                with self.assertRaisesRegex(RuntimeError, 'unfinished release'):
                    Deployment(self.runner, 'other-job').deploy(self.artifact, 'test')
                self.assertEqual(calls, len(self.runner.calls))
                with self.assertRaisesRegex(RuntimeError, 'unfinished release'):
                    self.runner.deploy(self.artifact)
                report = self.deployment.inspect()
                self.assertFalse(report['automatic_resume'])
                self.assertEqual(sum(a == action for _, a in self.mutations()), 1)
                self.assertNotIn('secret-must-not-be-logged', self.deployment.path.read_text())

    def test_backup_failure_keeps_old_binary_stopped_and_fenced(self):
        with patch.object(self.runner, 'backup', side_effect=OSError('disk full')):
            with self.assertRaises(OSError):
                self.run_deployment()
        self.assertEqual(self.runner.state, 'stopped')
        self.assertEqual(self.runner.entry.read_bytes(), b'old')
        self.assertTrue(self.runner.job_value['lower']['active'])
        self.assertNotIn('start', [a for _, a in self.mutations()])

    def test_bad_artifact_rejected_before_maintenance(self):
        (self.artifact / 'binary').write_bytes(b'corrupt')
        with self.assertRaises(RuntimeError):
            self.run_deployment()
        self.assertEqual(self.mutations(), [])

    def test_artifact_changed_during_backup_not_installed(self):
        backup = self.runner.backup
        def tamper(directory):
            result = backup(directory)
            (directory / 'binary').write_bytes(b'corrupt')
            return result
        with patch.object(self.runner, 'backup', side_effect=tamper):
            with self.assertRaises(RuntimeError):
                self.run_deployment()
        self.assertEqual(self.runner.entry.read_bytes(), b'old')
        self.assertEqual(self.runner.state, 'stopped')

    def test_wrong_lower_binding_or_unsupported_never_enter(self):
        self.runner.gateway = 'another-gateway'
        with self.assertRaisesRegex(RuntimeError, 'binding mismatch'):
            self.run_deployment()
        self.runner.gateway, self.runner.supported = 'gateway', False
        with self.assertRaisesRegex(RuntimeError, 'unsupported'):
            self.run_deployment()
        self.assertEqual(self.mutations(), [])

    def test_post_start_identity_mismatch_does_not_release(self):
        self.runner.fail = 'new-boot-wrong-owner'
        with self.assertRaisesRegex(RuntimeError, 'evidence'):
            self.run_deployment()
        self.assertNotIn('complete', [a for _, a in self.mutations()])

    def test_acceptance_failure_does_not_restore_or_release(self):
        self.runner.fail = 'accept'
        with self.assertRaises(RuntimeError):
            self.run_deployment()
        self.assertEqual(self.runner.entry.read_bytes(), b'new')
        self.assertNotIn('complete', [a for _, a in self.mutations()])

    def test_write_intent_failure_prevents_remote_mutation(self):
        from deployment import durable_json
        def fail_intent(path, data):
            if data.get('stages'):
                raise OSError('full')
            return durable_json(path, data)
        with patch('deployment.durable_json', side_effect=fail_intent):
            with self.assertRaises(OSError):
                self.run_deployment()
        self.assertEqual(self.mutations(), [])
        self.assertEqual(json.loads(marker_path(self.runner).read_text())['state'], 'running')

    def test_journal_cannot_be_reused_and_profile_drift_blocks_inspect(self):
        self.run_deployment()
        with self.assertRaisesRegex(RuntimeError, 'already recorded'):
            self.run_deployment()
        self.runner.p['gateway_instance_id'] = 'changed'
        with self.assertRaisesRegex(RuntimeError, 'binding changed'):
            self.deployment.inspect()

    def test_parallel_runner_lock_and_unsafe_identifiers(self):
        with target_lock(self.runner):
            with self.assertRaises(BlockingIOError):
                self.run_deployment()
        self.assertEqual(self.mutations(), [])
        for value in ('../job', '..', '', 'x' * 97):
            with self.assertRaises(ValueError):
                Deployment(self.runner, value)


if __name__ == '__main__':
    unittest.main()
