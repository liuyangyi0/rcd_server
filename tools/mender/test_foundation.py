"""Isolated tests: no services, OPC connections or serial devices are used."""
import copy
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from inventory import attributes, summarize, validate_profile
from package import digest, export, load_object, public_manifest
from preflight import assess, GIB


class PackageTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.binary = self.root / 'binary'
        self.binary.write_bytes(b'\x7fELF\x02\x01' + b'\x00' * 12 + b'\x3e\x00' + b'fixture-not-executable')
        self.source = {'component': 'manager', 'binary_sha256': digest(self.binary),
                       'source': {'sha256': 'b' * 64, 'files': {'private/path': 'not exported'}},
                       'host': '192.0.2.1', 'toolchain': 'private detail',
                       'build_info': {'schema_version': 1, 'component': 'manager',
                                      'version': '1.2.3', 'release_id': 'release-1',
                                      'source_sha256': 'b' * 64, 'commit': 'a' * 40,
                                      'dirty': True, 'built_at': '2026-10-08T00:00:00Z',
                                      'target': 'linux/amd64', 'secret': 'do not export'}}
        self.compatibility = {'adapter_min_version': '1.0.0',
                              'state_formats': {'manager_db': {'min': 15, 'max': 15}},
                              'component_dependencies': {}}

    def manifest(self):
        source = self.root / 'manifest.json'
        source.write_text(json.dumps(self.source))
        return public_manifest(self.root, digest(source), 'manager', ['rcd-linux-amd64'],
                               self.compatibility, 'test release')

    def test_public_metadata_contains_no_runtime_or_private_build_data(self):
        value = self.manifest()
        encoded = json.dumps(value)
        for secret in ('private/path', 'private detail', '192.0.2.1', 'do not export'):
            self.assertNotIn(secret, encoded)
        self.assertEqual(value['component'], 'manager')
        self.assertEqual(value['compatibility'], self.compatibility)

    def test_component_and_elf_and_provenance_are_checked(self):
        for field, bad in [('component', 'lower'), ('target', 'linux/arm64'),
                           ('source_sha256', 'c' * 64), ('version', '../escape'),
                           ('commit', 'unknown'), ('dirty', 'true'), ('built_at', '2026-10-08')]:
            with self.subTest(field=field):
                saved = self.source['build_info'][field]
                self.source['build_info'][field] = bad
                with self.assertRaises(ValueError):
                    self.manifest()
                self.source['build_info'][field] = saved

    def test_mutated_binary_and_symlink_are_rejected(self):
        self.binary.write_bytes(b'changed')
        with self.assertRaises(ValueError):
            self.manifest()
        self.binary.unlink()
        self.binary.symlink_to(self.root / 'missing')
        with self.assertRaises(ValueError):
            self.manifest()

    def test_explicit_state_compatibility_no_paths_or_booleans(self):
        for change in [{'state_formats': {}}, {'extra': 'unknown'},
                       {'state_formats': {'manager_db': {'min': True, 'max': 15}}},
                       {'state_formats': {'manager_db': {'min': 16, 'max': 15}}},
                       {'component_dependencies': {'../escape': '1.0.0'}}]:
            with self.subTest(change=change):
                saved = copy.deepcopy(self.compatibility)
                self.compatibility.update(change)
                with self.assertRaises(ValueError):
                    self.manifest()
                self.compatibility = saved

    def test_duplicate_json_field_rejected(self):
        path = self.root / 'duplicate.json'
        path.write_text('{"component":"lower","component":"manager"}')
        with self.assertRaisesRegex(ValueError, 'duplicate'):
            load_object(path)

    def test_pinned_manifest_required(self):
        self.manifest()
        with self.assertRaisesRegex(ValueError, 'SHA-256 mismatch'):
            public_manifest(self.root, 'f' * 64, 'manager', ['rcd-linux-amd64'],
                            self.compatibility, '')

    def test_export_only_copies_binary_and_public_metadata(self):
        manifest = self.manifest()
        tool = self.root / 'mender-artifact'
        tool.write_bytes(b'not executable; subprocess mocked')
        output = self.root / 'release.mender'
        def build(args, **kwargs):
            self.assertEqual(args[0], str(tool))
            self.assertEqual(args[args.index('--clears-provides') + 1], 'rcd.manager.*')
            files = [Path(args[i + 1]) for i, arg in enumerate(args) if arg == '-f']
            self.assertEqual([file.name for file in files], ['binary', 'rcd-manifest.json'])
            self.assertNotIn('private detail', files[1].read_text())
            Path(args[args.index('-o') + 1]).write_bytes(b'fake artifact for isolated test')
        with patch('package.subprocess.run', side_effect=build) as call:
            result = export(self.root, manifest, output, tool, digest(tool))
            self.assertEqual(result['sha256'], digest(output))
            self.assertEqual(call.call_count, 1)
            with self.assertRaisesRegex(ValueError, 'cannot be overwritten'):
                export(self.root, manifest, output, tool, digest(tool))
            self.assertEqual(call.call_count, 1)

    def test_changed_tool_and_binary_do_not_invoke_packer(self):
        manifest = self.manifest()
        tool = self.root / 'mender-artifact'
        tool.write_bytes(b'tool')
        with patch('package.subprocess.run') as call:
            with self.assertRaises(ValueError):
                export(self.root, manifest, self.root / 'a.mender', tool, 'f' * 64)
            self.binary.write_bytes(b'mutated after metadata validation')
            with self.assertRaisesRegex(ValueError, 'changed during export'):
                export(self.root, manifest, self.root / 'b.mender', tool, digest(tool))
            call.assert_not_called()


class PreflightTests(unittest.TestCase):
    def test_available_not_total_memory_and_no_unsafe_port_fallback(self):
        facts = {'system': 'Linux', 'machine': 'x86_64', 'cpu_flags': ['avx'],
                 'cpu_count': 16, 'memory_available_bytes': 28 * GIB,
                 'disk_free_bytes': 280 * GIB, 'occupied_ports': []}
        self.assertTrue(assess(facts)['resource_ready'])
        for field, value in [('cpu_flags', []), ('memory_available_bytes', 15 * GIB),
                             ('disk_free_bytes', 49 * GIB), ('occupied_ports', [8443])]:
            with self.subTest(field=field):
                self.assertFalse(assess(dict(facts, **{field: value}))['resource_ready'])


class InventoryTests(unittest.TestCase):
    def profile(self):
        binding = {'unit': 'example.service', 'scope': 'user', 'run_as': 'operator', 'instance': 'example'}
        return {'schema_version': 1, 'node_id': 'node-1', 'name': 'Example', 'address': '192.0.2.1',
                'services': {'manager': binding.copy(), 'lower-stack': binding.copy()}}

    def snapshot(self):
        return {'service': {'LoadState': 'loaded', 'ActiveState': 'active'},
                'processes': [{'entry': '/opt/rcd/bin/RcdManager', 'running': True,
                               'running_sha256': 'a' * 64,
                               'running_build': {'version': 'old', 'release_id': 'r1'},
                               'entry_build': {'version': 'new'},
                               'restart_required_for_entry': True}]}

    def test_reports_process_image_not_replacement_on_disk(self):
        result = summarize('manager', self.snapshot(), {}, 'now')
        self.assertEqual(result['running_version'], 'old')
        self.assertTrue(result['restart_required'])

    def test_offline_and_stopped_preserve_history_not_current_version(self):
        previous = summarize('manager', self.snapshot(), {}, 'previous')
        for snapshot, state in [(None, 'unknown'),
                                ({'service': {'LoadState': 'loaded', 'ActiveState': 'inactive'},
                                  'processes': []}, 'stopped')]:
            result = summarize('manager', snapshot, previous, 'now')
            self.assertEqual(result['state'], state)
            self.assertEqual(result['running_version'], '')
            self.assertEqual(result['last_confirmed_at'], 'previous')
            self.assertEqual(result['last_confirmed_build']['version'], 'old')

    def test_rebinding_does_not_reuse_stale_identity(self):
        profile = self.profile()
        _, previous = attributes(profile, {'manager': self.snapshot()}, {}, 'old-time')
        fields, _ = attributes(profile, {}, previous, 'now')
        self.assertEqual(fields['rcd_manager_last_version'], 'old')
        profile['services']['manager']['unit'] = 'different.service'
        fields, _ = attributes(profile, {}, previous, 'now')
        self.assertEqual(fields['rcd_manager_last_version'], '')

    def test_unknown_running_build_is_not_stopped_or_verified(self):
        snapshot = self.snapshot()
        snapshot['processes'][0]['running_build'] = None
        result = summarize('manager', snapshot, {}, 'now')
        self.assertEqual(result['state'], 'running_unverified')
        self.assertEqual(result['running_version'], '')

    def test_invalid_profile_is_rejected(self):
        profile = self.profile()
        validate_profile(profile)
        profile['services']['manager']['unit'] = 'x.service; reboot'
        with self.assertRaises(ValueError):
            validate_profile(profile)


if __name__ == '__main__':
    unittest.main()
