"""Remote-only controller acceptance. Uses temp paths, no systemd/serial controls.

RCD_ANSIBLE_PLAYBOOK and RCD_ANSIBLE_STAGE_BINARY opt in to executable checks.
The binary is only invoked with version --json, never as a service.
"""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


@unittest.skipUnless(os.getenv('RCD_ANSIBLE_PLAYBOOK') and os.getenv('RCD_ANSIBLE_STAGE_BINARY'),
                     'explicit isolated Ansible and version-only binary required')
class PlaybookTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='rcd-ansible-test-')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.ansible = os.environ['RCD_ANSIBLE_PLAYBOOK']
        self.playbooks = Path(__file__).resolve().parent
        self.host = subprocess.check_output(['hostname', '-I'], text=True).split()[0]
        self.artifact = self.root / 'controller-build'
        self.artifact.mkdir()
        shutil.copyfile(os.environ['RCD_ANSIBLE_STAGE_BINARY'], self.artifact / 'binary')
        info = json.loads(subprocess.check_output([os.environ['RCD_ANSIBLE_STAGE_BINARY'], 'version', '--json'], text=True))
        self.manifest = {'host': '192.0.2.46', 'component': info['component'], 'build_info': info,
                         'source': {'sha256': info['source_sha256']},
                         'binary_sha256': hashlib.sha256((self.artifact / 'binary').read_bytes()).hexdigest()}
        self.write(self.artifact / 'manifest.json', self.manifest)
        self.inventory = {'all': {'children': {'rcd_nodes': {'hosts': {}}}}}
        for name in ('first', 'second'):
            target = self.root / name
            target.mkdir()
            (target / 'runtime').mkdir()
            (target / 'source').mkdir()
            (target / 'bin').mkdir()
            self.write(target / 'profile.json', {
                'host': self.host, 'component': info['component'], 'source_dir': str(target / 'source'),
                'runtime_dir': str(target / 'runtime'), 'executable': str(target / 'bin/rcd'),
                'release_dir': str(target / 'releases')})
            self.inventory['all']['children']['rcd_nodes']['hosts'][name] = {
                'ansible_connection': 'local', 'ansible_host': self.host, 'ansible_python_interpreter': '/usr/bin/python3',
                'rcd_release': {'host': self.host, 'component': info['component'], 'profile': str(target / 'profile.json'),
                                'tool_dir': str(target / 'tools'), 'controller_artifact': str(self.artifact),
                                'incoming_dir': str(target / 'incoming'), 'artifact_dir': str(target / 'releases/staged'),
                                'manifest_sha256': hashlib.sha256((self.artifact / 'manifest.json').read_bytes()).hexdigest(),
                                'operation_id': 'test-operation', 'mode': 'manual'}}

    def write(self, path, data):
        path.write_text(json.dumps(data))

    def invoke(self, playbook, extra=(), expected=0, limit='first'):
        inventory = self.root / 'inventory.json'
        self.write(inventory, self.inventory)
        cmd = [self.ansible, '-i', str(inventory), str(self.playbooks / (playbook + '.yml'))]
        if limit:
            cmd += ['--limit', limit]
        result = subprocess.run(cmd + list(extra), capture_output=True, text=True, timeout=90)
        if expected == 0:
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        return result.stdout

    def test_syntax_all_entrypoints(self):
        for name in ('inspect', 'stage', 'deploy', 'verify', 'recover', 'rollback', 'semaphore-release'):
            self.invoke(name, ['--syntax-check'])

    def test_check_mode_writes_nothing(self):
        self.invoke('stage', ['--check'], limit='first,second')
        self.assertFalse((self.root / 'first/tools').exists())
        self.assertFalse((self.root / 'second/incoming').exists())

    def test_limit_is_mandatory_and_cannot_be_group_or_wildcard(self):
        for limit in (None, 'rcd_nodes', '*'):
            self.invoke('stage', expected=1, limit=limit)
        self.assertFalse((self.root / 'first/tools').exists())

    def test_stage_twice_unchanged_preserves_build_and_does_not_touch_runtime(self):
        self.invoke('stage')
        text = self.invoke('stage')
        self.assertRegex(text, r'changed=0\s')
        staged = self.root / 'first/releases/staged'
        self.assertEqual((staged / 'build-manifest.json').read_bytes(), (self.artifact / 'manifest.json').read_bytes())
        self.assertEqual(json.loads((staged / 'manifest.json').read_text())['host'], self.host)
        self.assertEqual(list((self.root / 'first/runtime').iterdir()), [])
        self.assertFalse((self.root / 'first/bin/rcd').exists())

    def test_first_failure_stops_later_hosts(self):
        self.inventory['all']['children']['rcd_nodes']['hosts']['first']['rcd_release']['manifest_sha256'] = 'a' * 64
        self.invoke('stage', expected=1, limit='first,second')
        self.assertFalse((self.root / 'second/tools').exists())

    def test_incoming_runtime_overlap_refused_before_file_transfer(self):
        self.inventory['all']['children']['rcd_nodes']['hosts']['first']['rcd_release']['incoming_dir'] = str(self.root / 'first/runtime')
        self.invoke('stage', expected=1)
        self.assertEqual(list((self.root / 'first/runtime').iterdir()), [])

    def test_deploy_without_current_authorization_refused_before_upload(self):
        self.invoke('deploy', expected=1)
        self.assertFalse((self.root / 'first/tools').exists())

    def test_failed_preflight_and_unknown_replay_do_not_create_running_binary(self):
        variables = self.root / 'window.json'
        self.write(variables, {'rcd_confirm_targets': [self.host + '/lower'], 'rcd_no_active_commands': True, 'rcd_initiator': 'isolated'})
        self.invoke('deploy', ['-e', '@' + str(variables)], expected=1)
        self.invoke('deploy', ['-e', '@' + str(variables)], expected=1)
        path = self.root / 'first/releases/ansible-operations/test-operation/operation.json'
        self.assertEqual(json.loads(path.read_text())['state'], 'unknown')
        self.assertFalse((self.root / 'first/bin/rcd').exists())


if __name__ == '__main__':
    unittest.main(verbosity=2)
