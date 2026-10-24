import copy
import json
import os
from pathlib import Path
import struct
import tarfile
import tempfile
import unittest
from unittest.mock import patch

import rcd_package


class RCDPackageTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='rcd-package-test-')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.build = self.root / 'build'
        self.build.mkdir()
        binary = bytearray(64)
        binary[:7] = b'\x7fELF\x02\x01\x01'
        struct.pack_into('<HHI', binary, 16, 2, 62, 1)
        struct.pack_into('<H', binary, 52, 64)
        (self.build / 'binary').write_bytes(binary)
        self.original = {'component': 'manager', 'source': {'sha256': 'b' * 64},
                         'binary_sha256': rcd_package.digest(self.build / 'binary'),
                         'private_path': '/secret/build', 'password': 'never-export',
                         'build_info': {'schema_version': 1, 'component': 'manager',
                                        'version': '1.0.0', 'release_id': 'test-001',
                                        'commit': 'a' * 40, 'source_sha256': 'b' * 64,
                                        'dirty': True, 'built_at': '2026-10-08T00:00:00Z',
                                        'target': 'linux/amd64'}}
        self.compatibility = {'adapter_min_version': '1.0.0', 'state_formats': {'manager_db': {'min': 0, 'max': 1}}, 'component_min_versions': {}}
        self.save()

    def save(self):
        (self.build / 'manifest.json').write_text(json.dumps(self.original))
        self.sha = rcd_package.digest(self.build / 'manifest.json')

    def export(self, name='release.rcd.tar.gz'):
        output = self.root / name
        rcd_package.export(self.build, self.sha, self.compatibility, '测试包，绝不执行', output)
        return output

    def test_public_deterministic_package(self):
        first, second = self.export(), self.export('second.rcd.tar.gz')
        self.assertEqual(first.read_bytes(), second.read_bytes())
        with tarfile.open(first) as archive:
            self.assertEqual(archive.getnames(), ['manifest.json', 'binary'])
            public = archive.extractfile('manifest.json').read()
            self.assertNotIn(b'never-export', public)
            self.assertNotIn(b'/secret/build', public)
            self.assertEqual(json.loads(public)['compatibility'], self.compatibility)
        if os.environ.get('RCD_PACKAGE_FIXTURE'):
            # Explicit remote-only interoperability output; same strict no-clobber builder.
            rcd_package.export(self.build, self.sha, self.compatibility, '测试包，绝不执行', Path(os.environ['RCD_PACKAGE_FIXTURE']))

    def test_no_overwrite(self):
        self.export()
        with self.assertRaises(ValueError):
            self.export()

    def test_external_single_binary_package_never_executes_or_exports_paths(self):
        info=dict(self.original['build_info']);info.pop('schema_version');info['component']='my-app'
        output=self.root/'my-app.rcd.tar.gz'
        with patch('subprocess.run',side_effect=AssertionError('never run uploaded binary')):
            rcd_package.export_binary(self.build/'binary',info,'registered generic application',output)
        with tarfile.open(output) as archive:
            m=json.loads(archive.extractfile('manifest.json').read())
            self.assertEqual(archive.getnames(),['manifest.json','binary'])
            self.assertEqual(m['component'],'my-app')
            self.assertEqual(m['compatibility']['state_formats'],{})
            self.assertNotIn('install_path',m)
        info['component']='manager'
        with self.assertRaises(ValueError):
            rcd_package.export_binary(self.build/'binary',info,'',self.root/'unsafe.rcd.tar.gz')

    def test_build_digest(self):
        self.sha = '0' * 64
        with self.assertRaises(ValueError):
            self.export()

    def test_binary_digest(self):
        (self.build / 'binary').write_bytes((self.build / 'binary').read_bytes() + b'changed')
        with self.assertRaises(ValueError):
            self.export()

    def test_no_links(self):
        (self.build / 'binary').rename(self.root / 'real-binary')
        (self.build / 'binary').symlink_to(self.root / 'real-binary')
        with self.assertRaises(ValueError):
            self.export()

    def test_compatibility_coverage(self):
        for compat in [dict(self.compatibility, state_formats={}),
                       dict(self.compatibility, component_min_versions={'manager': '1.0.0'}),
                       dict(self.compatibility, state_formats={'manager_db': {'min': 2, 'max': 1}}),
                       dict(self.compatibility, install_command='unsafe')]:
            with self.subTest(compat=compat), self.assertRaises(ValueError):
                rcd_package.manifest(self.build, self.sha, compat, '')

    def test_required_identity(self):
        original = copy.deepcopy(self.original)
        for key, value in [('target', 'linux/arm64'), ('built_at', '2026-10-08'),
                           ('commit', 'short'), ('dirty', None), ('version', '../../escape')]:
            self.original = copy.deepcopy(original)
            self.original['build_info'][key] = value
            self.save()
            with self.subTest(key=key), self.assertRaises(ValueError):
                self.export()

    def test_duplicate_json_fields(self):
        path = self.root / 'duplicate.json'
        path.write_text('{"component":"lower","component":"manager"}')
        with self.assertRaises(ValueError):
            rcd_package.load(path)

    def test_all_components_and_rust_architecture(self):
        for component in ('lower', 'manager', 'agent', 'communicate'):
            self.original['component'] = component
            self.original['build_info']['component'] = component
            self.original['build_info']['target'] = 'linux/x86_64' if component in ('lower', 'communicate') else 'linux/amd64'
            self.original['build_info']['version'] = '1.0.0+build'
            self.save()
            self.compatibility['state_formats'] = {name: {'min': 0, 'max': 1} for name in rcd_package.FORMATS[component]}
            with self.subTest(component=component):
                output = self.export(component + '.rcd.tar.gz')
                with tarfile.open(output) as archive:
                    public = json.load(archive.extractfile('manifest.json'))
                    self.assertEqual(public['component'], component)
                    self.assertEqual(public['target'], self.original['build_info']['target'])

    def test_communicate_does_not_inherit_lower_database_formats(self):
        self.original['component'] = 'communicate'
        self.original['build_info']['component'] = 'communicate'
        self.save()
        self.compatibility['state_formats'] = {}
        self.assertEqual(rcd_package.manifest(self.build, self.sha, self.compatibility, '')['component'], 'communicate')
        self.compatibility['state_formats'] = {'command_journal': {'min': 0, 'max': 1}}
        with self.assertRaisesRegex(ValueError, 'coverage'):
            self.export()


if __name__ == '__main__':
    unittest.main()
