import sqlite3
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from migrate import copy_verified, tree_manifest, check_databases, service_environment


class MigrationCopyTests(unittest.TestCase):
    def test_environment_rewrites_only_owned_paths(self):
        content='RCD_MANAGER_DATA_DIR=/old/data\nRCD_WEB_PASSWORD=keep-literal-value\nRCD_MANAGER_LOWER_SAMPLING_INTERVAL=50ms\n'
        self.assertEqual(service_environment(content,'manager','main'),
            'RCD_MANAGER_DATA_DIR=/var/lib/rcd/manager/main\nRCD_WEB_PASSWORD=keep-literal-value\nRCD_MANAGER_LOWER_SAMPLING_INTERVAL=50ms\n')
        content='RCD_LOWER_EXECUTABLE=/old/bin\nRCD_LOWER_WORK_DIR=/old/work\nRCD_AGENT_DATA_DIR=/old/state\nRCD_OPCUA_PORT=4841\n'
        value=service_environment(content,'lower-stack','example')
        self.assertNotIn('/old/',value)
        self.assertIn('RCD_OPCUA_PORT=4841\n',value)
        self.assertEqual(service_environment('','manager','main'),'')

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.source = self.root / 'source'
        self.source.mkdir()
        (self.source / 'user.toml').write_text('unchanged user configuration')

    def test_exact_copy_refuses_merge_and_keeps_source(self):
        before = tree_manifest(self.source)
        target = self.root / 'target'
        self.assertEqual(copy_verified(self.source, target), before)
        self.assertEqual(tree_manifest(self.source), before)
        with self.assertRaisesRegex(RuntimeError, 'already exists'):
            copy_verified(self.source, target)

    def test_symlink_is_not_silently_followed(self):
        (self.source / 'link').symlink_to(self.source / 'user.toml')
        with self.assertRaisesRegex(RuntimeError, 'symlink'):
            copy_verified(self.source, self.root / 'target')

    def test_concurrent_source_edit_is_rejected(self):
        original = tree_manifest
        calls = []
        def concurrent(path):
            calls.append(path)
            if len(calls) == 2:
                (self.source / 'user.toml').write_text('operator edit')
            return original(path)
        with patch('migrate.tree_manifest', side_effect=concurrent):
            with self.assertRaisesRegex(RuntimeError, 'changed'):
                copy_verified(self.source, self.root / 'target')
        self.assertEqual((self.source / 'user.toml').read_text(), 'operator edit')

    def test_sqlite_integrity_and_schema_without_changing_user_data(self):
        path = self.source / 'manager.db'
        with sqlite3.connect(path) as db:
            db.execute('PRAGMA user_version=14')
            db.execute('CREATE TABLE identity(id TEXT)')
            db.execute("INSERT INTO identity VALUES('existing-identity')")
        copy_verified(self.source, self.root / 'target')
        self.assertEqual(check_databases(self.root / 'target'), {'manager.db': 14})
        with sqlite3.connect(self.root / 'target/manager.db') as db:
            self.assertEqual(db.execute('SELECT id FROM identity').fetchone()[0], 'existing-identity')
        path.write_bytes(b'not a database')
        with self.assertRaises(sqlite3.DatabaseError):
            check_databases(self.source)
