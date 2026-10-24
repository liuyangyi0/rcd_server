import copy
import unittest
from filter_plugins.semaphore_release import resolve


class SurveyTests(unittest.TestCase):
    def setUp(self):
        self.host = '192.0.2.46'
        self.catalog = {'targets': {self.host: {'components': {'lower': {
            'release_dir': '/var/lib/rcd-release/lower', 'profile': '/etc/rcd/lower.json'}}}},
            'packages': {'v1': {'lower': {'path': '/opt/packages/v1/lower', 'manifest_sha256': 'a' * 64}}}}
        self.values = {'component': 'lower', 'release_id': 'v1'}

    def test_default_is_preview(self):
        result = resolve(self.values, self.catalog, self.host, 'deploy')
        self.assertTrue(result['preview'])
        self.assertFalse(result['no_active_commands'])

    def test_execution_requires_all_confirmation_fields(self):
        values = dict(self.values, execution='execute', command_pause='confirmed', operation_id='job-1', initiator='tester')
        for field in ('command_pause', 'operation_id', 'initiator'):
            invalid = dict(values)
            invalid.pop(field)
            with self.assertRaises(ValueError):
                resolve(invalid, self.catalog, self.host, 'deploy')
        result = resolve(values, self.catalog, self.host, 'deploy')
        self.assertFalse(result['preview'])
        self.assertEqual(result['confirm_targets'], [self.host + '/lower'])

    def test_unknown_package_component_host_rejected(self):
        for field, value in [('release_id', '../x'), ('component', 'manager')]:
            with self.assertRaises((ValueError, KeyError)):
                resolve(dict(self.values, **{field: value}), self.catalog, self.host, 'stage')
        with self.assertRaises(KeyError):
            resolve(self.values, self.catalog, '192.0.2.58', 'stage')

    def test_rollback_paths_cannot_escape(self):
        for ref in ('/tmp/backup', '../backup', 'artifacts/../backup-' + 'a' * 32,
                    'artifacts/v1/../../backup-' + 'a' * 32):
            with self.assertRaises(ValueError):
                resolve(dict(self.values, backup_ref=ref), self.catalog, self.host, 'rollback')
        result = resolve(dict(self.values, backup_ref='artifacts/v1/backup-' + 'a' * 32), self.catalog, self.host, 'rollback')
        self.assertFalse(result['restore_data'])

    def test_recovery_uses_original_transaction_and_no_fallback(self):
        with self.assertRaises(ValueError):
            resolve(dict(self.values, recovery_action='resume-deployment'), self.catalog, self.host, 'recover')
        result = resolve(dict(self.values, recovery_action='finish-rollback', deployment_id='original'), self.catalog, self.host, 'recover')
        self.assertEqual(result['release']['mode'], 'maintenance')
        self.assertEqual(result['release']['deployment_id'], 'original')
        self.assertTrue(result['preview'])

    def test_readonly_and_stage_do_not_require_pause(self):
        for action in ('stage', 'history', 'preflight', 'verify-running'):
            result = resolve(self.values, self.catalog, self.host, action)
            self.assertFalse(result['no_active_commands'])
            self.assertFalse(result['preview'])


if __name__ == '__main__':
    unittest.main()
