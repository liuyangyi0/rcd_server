import json
from pathlib import Path
import sqlite3
import tempfile
import unittest
from release import Release, sha, source_manifest, build_identity, identity_environment, identity_ldflags, require_manager_web_sources


class IsolatedRelease(Release):
    def local_host(self):
        pass  # Only a test seam. Never invokes systemctl or a process agent.

    def preflight(self):
        return {'isolated': True}

    def control(self, action):
        self.actions.append(action)

    def accept(self, expected):
        assert sha(self.entry) == expected
        return {'isolated': True, 'sha256': expected}


class ReleaseTests(unittest.TestCase):
    def test_web_release_refuses_partial_source_or_missing_screenshots(self):
        with self.assertRaisesRegex(RuntimeError, 'incomplete web source snapshot'):
            require_manager_web_sources(self.source)
        web = self.source / 'manager/web'
        files = {
            'src/App.tsx': 'const HelpPage = lazy(() => import("./pages/HelpPage"));',
            'src/components/AppShell.tsx': "{ to: '/help', label: '帮助中心' }",
            'src/pages/HelpPage.tsx': 'page', 'src/pages/HelpPage.css': 'css',
            'src/features/help/content.ts': 'content',
            'src/features/help/screenshots.ts': "[{ id: 'manager-points' }]",
            'src/features/help/RelationshipDiagram.tsx': 'diagram',
            'embed_help_test.go': 'package webassets',
        }
        for name, content in files.items():
            target = web / name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(content)
        with self.assertRaisesRegex(RuntimeError, 'missing help screenshot'):
            require_manager_web_sources(self.source)
        image = web / 'public/help/screenshots/manager-points.jpg'
        image.parent.mkdir(parents=True)
        image.write_bytes(b'not a JPEG')
        with self.assertRaisesRegex(RuntimeError, 'invalid help screenshot'):
            require_manager_web_sources(self.source)
        image.write_bytes(b'\xff\xd8\xfffixture')
        self.assertEqual(require_manager_web_sources(self.source), 1)

    def test_source_manifest_covers_shared_rust_crates(self):
        shared = self.source / 'crates' / 'rcd-config-form'
        (shared / 'src').mkdir(parents=True)
        (shared / 'Cargo.toml').write_text('[package]\nname="rcd-config-form"\n')
        code = shared / 'src' / 'lib.rs'
        code.write_text('// initial\n')
        initial = source_manifest(self.source)
        self.assertIn('crates/rcd-config-form/src/lib.rs', initial['files'])
        code.write_text('// modified\n')
        self.assertNotEqual(source_manifest(self.source)['sha256'], initial['sha256'])

    def test_source_manifest_covers_independent_communication_program(self):
        project = self.source / 'communicate'
        (project / 'src').mkdir(parents=True)
        (project / 'Cargo.toml').write_text('[package]\nname="rcd_communicate"\n')
        code = project / 'src/main.rs'
        code.write_text('// before\n')
        first = source_manifest(self.source)
        self.assertIn('communicate/src/main.rs', first['files'])
        code.write_text('// changed\n')
        self.assertNotEqual(source_manifest(self.source)['sha256'], first['sha256'])
        (project / 'target').mkdir()
        (project / 'target/generated').write_text('not source')
        self.assertNotIn('communicate/target/generated', source_manifest(self.source)['files'])

    def test_communicate_is_independent_and_legacy_restarts_are_refused(self):
        profile = dict(self.profile, component='communicate', databases=[],
                       executable=str(self.runtime/'rcd_communicate'),
                       control={'kind': 'systemd-user', 'unit': 'rcd-communicate.service'})
        release = Release(profile, profile['host'], 'communicate')
        self.assertEqual(release.p['component'], 'communicate')
        with self.assertRaisesRegex(RuntimeError, 'controlled single-component'):
            release.guard_agent_maintenance_state()
        for invalid in [dict(profile, executable=str(self.runtime/'rcd_server')),
                        dict(profile, control={'kind':'systemd-user','unit':'rcd-lower.service'}),
                        dict(profile, control={'kind':'process-agent'})]:
            with self.assertRaisesRegex(ValueError, 'independent'):
                Release(invalid, profile['host'], 'communicate')

    def test_communication_stop_evidence_rejects_running_restarting_or_unbound_services(self):
        from unittest.mock import patch
        profile = dict(self.profile, component='communicate', databases=[],
                       executable=str(self.runtime/'rcd_communicate'),
                       control={'kind': 'systemd-user', 'unit': 'rcd-communicate.service'})
        release = Release(profile, profile['host'], 'communicate')
        for state, allowed in [
            ('LoadState=loaded\nActiveState=inactive\nSubState=dead\nMainPID=0\nJob=', True),
            ('LoadState=loaded\nActiveState=failed\nSubState=failed\nMainPID=0\nJob=0', True),
            ('LoadState=loaded\nActiveState=active\nSubState=running\nMainPID=9\nJob=', False),
            ('LoadState=loaded\nActiveState=activating\nSubState=auto-restart\nMainPID=0\nJob=', False),
            ('LoadState=not-found\nActiveState=inactive\nSubState=dead\nMainPID=0\nJob=', False),
            ('LoadState=loaded\nActiveState=inactive\nSubState=dead\nMainPID=0\nJob=10', False),
        ]:
            with self.subTest(state=state), patch.object(release, 'systemctl', return_value=state):
                if allowed: release.require_stopped_communicate()
                else:
                    with self.assertRaisesRegex(RuntimeError, '排空'):
                        release.require_stopped_communicate()
        release.p['control'] = {'kind': 'process-agent'}
        with self.assertRaisesRegex(RuntimeError, 'own fixed systemd'):
            release.require_stopped_communicate()

    def test_communicate_does_not_silently_ignore_state_databases(self):
        release = Release(dict(self.profile, component='communicate', executable=str(self.runtime/'rcd_communicate')), self.profile['host'], 'communicate')
        with self.assertRaisesRegex(RuntimeError, 'no supported database'):
            release.validate_state_inventory()

    def test_running_agent_acceptance_does_not_require_stopped_child_opc_port(self):
        from unittest.mock import patch
        import io
        profile = dict(self.profile, component='agent', health_url='http://127.0.0.1:8090/healthz')
        release = Release(profile, profile['host'], 'agent')
        with patch.object(release, 'pid', return_value=42), patch('release.sha', return_value='hash'), \
             patch('release.urllib.request.urlopen', return_value=io.BytesIO(b'{"ok":true}')), \
             patch('release.socket.create_connection') as opc:
            result = release.accept('hash')
        self.assertTrue(result['agent_http_ready'])
        opc.assert_not_called()

    def test_dynamic_config_inventory_stays_inside_runtime(self):
        config = self.runtime / 'config'
        config.mkdir()
        (config / 'first.toml').write_text('first')
        profile = dict(self.profile, config_dirs=[str(config)])
        first = Release(profile, profile['host'], 'manager')
        self.assertIn(config / 'first.toml', first.configs)
        (config / 'user-added.csv').write_text('new')
        second = Release(profile, profile['host'], 'manager')
        self.assertIn(config / 'user-added.csv', second.configs)
        (config / 'escape').symlink_to(self.source)
        with self.assertRaisesRegex(ValueError, 'symlinks'):
            Release(profile, profile['host'], 'manager')

    def test_user_systemd_uses_explicit_user_bus_when_publishing_as_root(self):
        from unittest.mock import patch
        from types import SimpleNamespace
        profile = dict(self.profile, control={'kind': 'systemd-user', 'run_as': 'operator', 'unit': 'rcd-manager.service'})
        release = Release(profile, profile['host'], 'manager')
        with patch('os.geteuid', return_value=0), patch('pwd.getpwnam', return_value=SimpleNamespace(pw_uid=1234)), patch('release.run', return_value='55') as run:
            self.assertEqual(release.pid(), 55)
            args = run.call_args.args
            self.assertEqual(args[:4], ('runuser', '-u', 'operator', '--'))
            self.assertIn('DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1234/bus', args)
        with self.assertRaises(ValueError):
            Release(dict(profile, control=dict(profile['control'], run_as='x;id')), profile['host'], 'manager')

    def test_maintenance_state_cannot_be_omitted_from_backup_inventory(self):
        state = self.runtime / 'state'
        state.mkdir()
        maintenance = state / 'maintenance.sqlite'
        with sqlite3.connect(maintenance) as db:
            db.execute('CREATE TABLE maintenance_state(record TEXT)')
        with self.assertRaisesRegex(RuntimeError, 'required state database'):
            self.runner.validate_state_inventory()
        with self.assertRaisesRegex(RuntimeError, 'required state database'):
            self.runner.backup(self.build)
        self.runner.databases.append(maintenance)
        self.runner.validate_state_inventory()
        backup = self.runner.backup(self.build)
        record = json.loads((backup / 'backup.json').read_text())
        self.assertTrue(any(item['target'] == str(maintenance) and item['database'] for item in record['files']))

    def test_build_identity_maps_to_both_languages(self):
        identity = build_identity('a' * 40, 'b' * 64, 'rcd-test-1', '1.0.0-test')
        env = identity_environment(identity)
        self.assertEqual(env['RCD_BUILD_COMMIT'], identity['commit'])
        self.assertEqual(env['RCD_BUILD_VERSION'], identity['version'])
        self.assertEqual(env['RCD_SOURCE_SHA256'], identity['source_sha256'])
        self.assertEqual(env['RCD_BUILD_DIRTY'], 'true')
        flags = identity_ldflags(identity)
        self.assertIn('buildinfo.Version=1.0.0-test', flags)
        self.assertIn('buildinfo.ReleaseID=rcd-test-1', flags)
        self.assertIn('buildinfo.Dirty=true', flags)

    def test_build_identifier_rejects_flag_injection(self):
        for value in ['', 'has space', '../release', 'x -X other=value', 'x' * 129]:
            with self.assertRaises(ValueError):
                build_identity('a' * 40, 'b' * 64, value, '1.0.0')
            with self.assertRaises(ValueError):
                build_identity('a' * 40, 'b' * 64, 'release', value)

    def test_agent_is_a_separate_release_role(self):
        profile = dict(self.profile, component='agent', databases=[])
        runner = IsolatedRelease(profile, profile['host'], 'agent')
        self.assertEqual(runner.p['component'], 'agent')
        with self.assertRaises(ValueError):
            IsolatedRelease(profile, profile['host'], 'manager')

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        self.runtime = root / 'runtime'
        self.source = root / 'source'
        self.runtime.mkdir()
        self.source.mkdir()
        self.binary = self.runtime / 'manager'
        self.binary.write_bytes(b'previous-not-executable')
        self.db = self.runtime / 'manager.db'
        with sqlite3.connect(self.db) as db:
            db.execute('PRAGMA user_version=1')
            db.execute('CREATE TABLE data(value TEXT)')
            db.execute("INSERT INTO data VALUES('original')")
        self.profile = {'host': '192.0.2.1', 'component': 'manager', 'source_dir': str(self.source),
                        'release_dir': str(root / 'releases'), 'runtime_dir': str(self.runtime),
                        'executable': str(self.binary), 'databases': [str(self.db)]}
        self.runner = IsolatedRelease(self.profile, '192.0.2.1', 'manager')
        self.runner.actions = []
        self.build = root / 'releases/build-test'
        self.build.mkdir(parents=True)
        (self.build / 'binary').write_bytes(b'new-not-executable')
        (self.build / 'manifest.json').write_text(json.dumps({'host': '192.0.2.1', 'component': 'manager', 'binary_sha256': sha(self.build / 'binary')}))

    def test_rollback_pairs_binary_and_database_and_preserves_failed_deployment(self):
        self.runner.deploy(self.build)
        with sqlite3.connect(self.db) as db:
            db.execute('PRAGMA user_version=99')
            db.execute("UPDATE data SET value='new'")
        backup = next(self.build.glob('backup-*'))
        self.runner.rollback(backup, restore_data=True)
        self.assertEqual(self.binary.read_bytes(), b'previous-not-executable')
        with sqlite3.connect(self.db) as db:
            self.assertEqual(db.execute('PRAGMA user_version').fetchone()[0], 1)
            self.assertEqual(db.execute('SELECT value FROM data').fetchone()[0], 'original')
        self.assertEqual(len(list(self.build.glob('backup-*'))), 2)
        self.assertEqual(self.runner.actions, ['stop', 'start', 'stop', 'start'])

    def test_default_rollback_preserves_current_user_data(self):
        self.runner.deploy(self.build)
        with sqlite3.connect(self.db) as db:
            db.execute("UPDATE data SET value='user edit after deployment'")
        backup = next(self.build.glob('backup-*'))
        self.runner.rollback(backup)
        self.assertEqual(self.binary.read_bytes(), b'previous-not-executable')
        with sqlite3.connect(self.db) as db:
            self.assertEqual(db.execute('SELECT value FROM data').fetchone()[0], 'user edit after deployment')

    def test_default_rollback_rejects_schema_change_without_overwriting(self):
        self.runner.deploy(self.build)
        with sqlite3.connect(self.db) as db:
            db.execute('PRAGMA user_version=99')
        backup = next(self.build.glob('backup-*'))
        with self.assertRaisesRegex(RuntimeError, 'schema changed'):
            self.runner.rollback(backup)
        self.assertEqual(self.binary.read_bytes(), b'new-not-executable')
        self.assertEqual(self.runner.actions, ['stop', 'start', 'stop'])
        with sqlite3.connect(self.db) as db:
            self.assertEqual(db.execute('PRAGMA user_version').fetchone()[0], 99)

    def test_wrong_hash_is_rejected_before_stop(self):
        (self.build / 'binary').write_bytes(b'changed')
        with self.assertRaises(RuntimeError):
            self.runner.deploy(self.build)
        self.assertEqual(self.runner.actions, [])

    def test_target_mismatch_is_rejected(self):
        with self.assertRaises(ValueError):
            Release(self.profile, '192.0.2.2', 'manager')

    def test_maintenance_generation_restore_is_rejected_before_stop(self):
        maintenance = self.runtime / 'maintenance.sqlite'
        with sqlite3.connect(maintenance) as db:
            db.execute('CREATE TABLE generation(value TEXT)')
        self.runner.databases.append(maintenance)
        backup = self.runner.backup(self.build)
        with self.assertRaisesRegex(RuntimeError, 'generation cannot be restored'):
            self.runner.rollback(backup, restore_data=True)
        self.assertEqual(self.runner.actions, [])

    def test_agent_v2_rollback_requires_compatibility_plan_before_stop(self):
        state = self.runtime / 'process-state.json'
        state.write_text(json.dumps({'schema_version': 2, 'maintenance': {'phase': 'completed'}}))
        profile = dict(self.profile, component='agent', databases=[], config_files=[str(state)])
        runner = IsolatedRelease(profile, profile['host'], 'agent')
        runner.actions = []
        backup = runner.backup(self.build)
        with self.assertRaisesRegex(RuntimeError, 'compatibility/recovery plan'):
            runner.rollback(backup)
        with self.assertRaisesRegex(RuntimeError, 'compatibility/recovery plan'):
            runner.deploy(self.build)
        self.assertEqual(runner.actions, [])

    def test_agent_state_inventory_cannot_be_omitted(self):
        runner = IsolatedRelease(dict(self.profile, component='agent', databases=[]), self.profile['host'], 'agent')
        with self.assertRaisesRegex(RuntimeError, 'process-state.json'):
            runner.validate_state_inventory()

    def test_source_digest_records_dirty_contents(self):
        (self.source / 'src').mkdir()
        file = self.source / 'src/main.rs'
        file.write_text('before')
        before = source_manifest(self.source)
        file.write_text('after')
        self.assertNotEqual(before['sha256'], source_manifest(self.source)['sha256'])


if __name__ == '__main__':
    unittest.main()
