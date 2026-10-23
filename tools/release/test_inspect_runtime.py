import json
from pathlib import Path
import tempfile
import types
import unittest
from unittest import mock

from inspect_runtime import digest, known_images, image_inventory, inspect, inspect_host
from publish_identity import public_identity


class RuntimeInventoryTests(unittest.TestCase):
    def test_host_batch_adds_communicate_and_uses_one_identity_scan(self):
        services = [{'component':'manager','scope':'user','unit':'rcd-manager.service','instance':'manager','run_as':'operator'},
                    {'component':'lower-stack','scope':'user','unit':'rcd-agent.service','instance':'lower','run_as':'operator'}]
        calls=[]
        def observed(component,*args):
            calls.append(component)
            return {'component':component}
        with mock.patch('inspect_runtime.verified_images',return_value={}) as images, \
                mock.patch('inspect_runtime.inspect',side_effect=observed), \
                mock.patch.dict('sys.modules', {'host_metrics':types.SimpleNamespace(collect_host_status=lambda:{'available':True})}):
            result=inspect_host(services, [], [{'component':'sample-app'}])
        self.assertEqual(calls,['manager','lower-stack','communicate','sample-app'])
        self.assertEqual(len(services),2)
        images.assert_called_once()
        self.assertTrue(result['host_status']['available'])

    def test_metrics_failure_does_not_discard_software(self):
        services=[{'component':'manager','scope':'user','unit':'rcd-manager.service','instance':'manager','run_as':'operator'},
                  {'component':'lower-stack','scope':'user','unit':'rcd-agent.service','instance':'lower','run_as':'operator'}]
        with mock.patch('inspect_runtime.verified_images',return_value={}), \
                mock.patch('inspect_runtime.inspect',side_effect=PermissionError('private details')), \
                mock.patch.dict('sys.modules',{'host_metrics':types.SimpleNamespace(collect_host_status=mock.Mock(side_effect=OSError('unavailable')))}):
            result=inspect_host(services,[])
        self.assertEqual(len(result['probes']),3)
        self.assertIsNone(result['probes'][0]['processes'][0]['running'])
        self.assertFalse(result['host_status']['available'])
        self.assertNotIn('private details',json.dumps(result))

    def test_not_installed_requires_absent_service_and_entry(self):
        show=types.SimpleNamespace(returncode=1,stdout='LoadState=not-found\nActiveState=inactive\nMainPID=0\n')
        with mock.patch('inspect_runtime.subprocess.run',return_value=show), \
                mock.patch('inspect_runtime.image_inventory',return_value={'component':'communicate','running':False,'entry_error':'absent'}), \
                mock.patch('inspect_runtime.Path.lstat',side_effect=FileNotFoundError):
            result=inspect('communicate','rcd-communicate.service','system','test',images={})
        self.assertIs(result['processes'][0]['installed'],False)
        changed=types.SimpleNamespace(returncode=0,stdout='LoadState=loaded\nActiveState=active\nMainPID=100\n')
        with mock.patch('inspect_runtime.subprocess.run',side_effect=[show,changed]), \
                mock.patch('inspect_runtime.image_inventory',return_value={'component':'communicate','running':False}), \
                mock.patch('inspect_runtime.Path.lstat',side_effect=FileNotFoundError):
            result=inspect('communicate','rcd-communicate.service','system','test',images={})
        self.assertIsNone(result['processes'][0]['running'])
        self.assertIn('inspection_error',result)

    def test_public_identity_is_verified_and_excludes_private_metadata(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);release=root/'release';release.mkdir()
            binary=release/'RcdManager';binary.write_bytes(b'image')
            (release/'manifest.json').write_text(json.dumps({'source':{'private':'not published'},'components':{'RcdManager':{'sha256':digest(binary),'build_info':{'version':'v1','password':'not published'}}}}))
            public=public_identity(release)
            self.assertNotIn('source',public)
            self.assertNotIn('password',public['components']['RcdManager']['build_info'])
            (release/'identity-public.json').write_text(json.dumps(public))
            (release/'manifest.json').unlink()
            self.assertEqual(known_images(root)[('RcdManager',digest(binary))]['version'],'v1')
            binary.write_bytes(b'changed')
            self.assertEqual(known_images(root),{})

    def test_disk_update_is_not_reported_as_running_version(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory)
            old=root/'old';old.write_bytes(b'old image')
            entry=root/'RcdManager';entry.write_bytes(b'new image')
            proc=root/'exe';proc.symlink_to(old)
            images={('RcdManager',digest(old)):{'version':'old'},('RcdManager',digest(entry)):{'version':'new'}}
            result=image_inventory(entry,proc,'RcdManager',images)
            self.assertTrue(result['restart_required_for_entry'])
            self.assertEqual(result['running_build']['version'],'old')
            self.assertEqual(result['entry_build']['version'],'new')

    def test_stopped_and_unknown_are_not_reported_as_running(self):
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/'RcdManager';path.write_bytes(b'unknown')
            result=image_inventory(path,None,'RcdManager',{})
            self.assertFalse(result['running'])
            self.assertIsNone(result['entry_build'])
            self.assertNotIn('running_build',result)
            result=image_inventory(path,path.parent/'missing-exe','RcdManager',{})
            self.assertIsNone(result['running'])
            self.assertIn('running_error',result)

    def test_corrupt_artifact_does_not_supply_identity(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);release=root/'release';release.mkdir()
            binary=release/'RcdManager';binary.write_bytes(b'original')
            (release/'manifest.json').write_text(json.dumps({'components':{'RcdManager':{'sha256':digest(binary),'build_info':{'version':'verified'}}}}))
            self.assertEqual(len(known_images(root)),1)
            binary.write_bytes(b'changed')
            self.assertEqual(known_images(root),{})

    def test_inventory_never_returns_private_build_fields(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory); release = root/'r1'; release.mkdir()
            binary = release/'RcdManager'; binary.write_bytes(b'image')
            (release/'manifest.json').write_text(json.dumps({'components': {'RcdManager': {
                'sha256': digest(binary), 'build_info': {'version': 'v1', 'password': 'secret'}}}}))
            self.assertEqual(known_images(root)[('RcdManager', digest(binary))], {'version': 'v1'})

    def test_conflicting_verified_identity_is_unknown(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for version in ('one', 'two'):
                release = root/version; release.mkdir()
                binary = release/'RcdManager'; binary.write_bytes(b'same bytes')
                (release/'manifest.json').write_text(json.dumps({'components': {'RcdManager': {
                    'sha256': digest(binary), 'build_info': {'version': version}}}}))
            self.assertEqual(known_images(root), {})

    def test_image_changed_mid_probe_does_not_keep_a_version(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory); old = root/'old'; old.write_bytes(b'old')
            new = root/'new'; new.write_bytes(b'new'); proc = root/'exe'; proc.symlink_to(old)
            actual_digest = digest
            def replaced(path):
                value = actual_digest(path)
                if path == proc:
                    proc.unlink(); proc.symlink_to(new)
                return value
            with mock.patch('inspect_runtime.digest', side_effect=replaced):
                result = image_inventory(old, proc, 'RcdManager', {('RcdManager', digest(old)): {'version': 'old'}})
            self.assertIsNone(result['running'])
            self.assertNotIn('running_build', result)
            self.assertNotIn('running_sha256', result)
