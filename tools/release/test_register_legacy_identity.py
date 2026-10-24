import json
from pathlib import Path
import tempfile
import unittest

from inspect_runtime import known_images, image_inventory
from register_legacy_identity import digest, identity, register


class LegacyIdentityTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.source = self.root / 'source/communicate'
        (self.source / 'src').mkdir(parents=True)
        (self.source / 'Cargo.toml').write_text('[package]\nname = "rcd_communicate"\nversion = "0.2.0"\n')
        (self.source / 'Cargo.lock').write_text('original dependency lock\n')
        (self.source / 'src/main.rs').write_text('original source\n')
        self.checksums = self.root / 'source-files.sha256'
        self.checksums.write_text(''.join(digest(p) + '  ' + str(p.relative_to(self.root)) + '\n'
                                         for p in sorted(self.source.rglob('*')) if p.is_file()))
        self.base = self.root / 'base.json'
        self.base.write_text(json.dumps({'declared_base_commit': 'a' * 40}))
        self.binary = self.root / 'original-image'
        self.binary.write_bytes(b'\x7fELF\x02\x01\x01' + b'\0' * 11 + b'\x3e\0' + b'original image')
        self.releases = self.root / 'releases'
        self.releases.mkdir()

    def record(self):
        return identity(self.binary, digest(self.binary), self.root, self.checksums,
                        digest(self.checksums), self.base, digest(self.base),
                        'legacy-communicate-20261020-test')

    def test_hash_backed_registration_and_idempotency(self):
        record = self.record()
        first = register(record, self.binary, self.binary, self.releases)
        self.assertTrue(first['changed'])
        self.assertFalse(register(record, self.binary, self.binary, self.releases)['changed'])
        images = known_images(self.releases)
        process_image = self.root / 'process-image'
        process_image.symlink_to(self.binary)
        view = image_inventory(self.binary, process_image, 'rcd_communicate', images)
        self.assertEqual(view['running_build']['version'], '0.2.0')
        self.assertEqual(view['running_build'], view['entry_build'])
        self.assertEqual(view['running_build']['built_at'], '')
        self.assertFalse(record['provenance']['embedded_build_identity'])
        self.assertFalse(record['provenance']['deployment_performed'])
        self.assertFalse(view['restart_required_for_entry'])

    def test_corrupt_original_evidence_is_rejected(self):
        expected = digest(self.checksums)
        self.checksums.write_text('corrupt')
        with self.assertRaisesRegex(ValueError, 'checksum mismatch'):
            identity(self.binary, digest(self.binary), self.root, self.checksums, expected,
                     self.base, digest(self.base), 'legacy-communicate-test')

    def test_modified_source_is_not_registered(self):
        (self.source / 'src/main.rs').write_text('changed source')
        with self.assertRaisesRegex(ValueError, 'checksum mismatch'):
            self.record()

    def test_path_traversal_and_duplicate_sources_are_rejected(self):
        for suffix in ['source/../../outside', 'source/communicate/Cargo.lock']:
            with self.subTest(path=suffix):
                original = self.checksums.read_text()
                self.checksums.write_text(original + '0' * 64 + '  ' + suffix + '\n')
                with self.assertRaisesRegex(ValueError, 'unsafe or duplicate'):
                    self.record()
                self.checksums.write_text(original)

    def test_other_running_image_is_not_registered(self):
        other = self.root / 'other'
        other.write_bytes(b'other image')
        with self.assertRaisesRegex(ValueError, 'running image'):
            register(self.record(), self.binary, other, self.releases)
        self.assertEqual(list(self.releases.iterdir()), [])

    def test_immutable_registration_conflicts_are_not_overwritten(self):
        record = self.record()
        result = register(record, self.binary, self.binary, self.releases)
        record['components']['rcd_communicate']['build_info']['version'] = '9.9.9'
        with self.assertRaisesRegex(ValueError, 'immutable registration conflict'):
            register(record, self.binary, self.binary, self.releases)
        saved = json.loads((Path(result['directory']) / 'identity-public.json').read_text())
        self.assertEqual(saved['components']['rcd_communicate']['build_info']['version'], '0.2.0')

    def test_modified_archive_cannot_supply_a_version(self):
        result = register(self.record(), self.binary, self.binary, self.releases)
        artifact = Path(result['directory']) / 'rcd_communicate'
        artifact.chmod(0o644)
        artifact.write_bytes(b'changed archive')
        self.assertEqual(known_images(self.releases), {})
