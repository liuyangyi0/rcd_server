"""Fixture-only onboarding tests. Never touch a real service or /opt entry."""
import base64
from contextlib import ExitStack, nullcontext
import hashlib
import io
import json
import os
import stat
from pathlib import Path
from types import SimpleNamespace
import tarfile
import tempfile
import unittest
from unittest.mock import patch

import host_setup as setup


def request():
    return {'schema_version': 1, 'operation_id': 'a' * 64, 'project_id': 1, 'task_id': 12,
            'address': '192.0.2.10', 'ssh_port': 22, 'service_account': 'operator'}


def application():
    return {'component': 'demo', 'name': 'Demo', 'revision': 1, 'arguments': ['--quiet'],
            'graceful_shutdown_confirmed': True, 'service_user': 'device_owner',
            'install_path': '/opt/rcd/apps/demo/bin/demo', 'work_dir': '/var/lib/rcd/apps/demo',
            'service_unit': 'rcd-app-demo.service', 'version_mode': 'manifest_sha256',
            'definition_sha256': 'b' * 64}


class HostSetupTests(unittest.TestCase):
    def test_identity_rejects_arbitrary_target_and_root_owner(self):
        for update in ({'operation_id': '../bad'}, {'address': 'host; sh'}, {'ssh_port': 0},
                       {'task_id': True}, {'service_account': 'root'}, {'service_account': 'u\nExecStart=x'}):
            with self.subTest(update=update), self.assertRaises(ValueError):
                setup.request_identity(dict(request(), **update))

    def test_keyscan_is_fixed_and_sends_no_credentials(self):
        raw = b'fixture public host key bytes'
        public = base64.b64encode(raw).decode()
        output = '192.0.2.10 ssh-ed25519 ' + public + '\n'
        def run(argv, **kwargs):
            self.assertEqual(argv[1:], ['-T', '5', '-p', '22', '-t', 'ed25519,ecdsa,rsa', '192.0.2.10'])
            self.assertNotIn('shell', kwargs)
            self.assertEqual(kwargs['timeout'], 20)
            return SimpleNamespace(stdout=output, stderr='', returncode=0)
        with patch.object(Path, 'is_file', return_value=True):
            report = setup.keyscan(request(), run=run)
        self.assertEqual(report['state'], 'awaiting_host_key')
        self.assertEqual(report['host_keys'][0]['fingerprint'], 'SHA256:' + base64.b64encode(hashlib.sha256(raw).digest()).decode().rstrip('='))

    def test_keyscan_rejects_wrong_host_duplicate_algorithm_and_empty(self):
        public = base64.b64encode(b'fixture public host key bytes').decode()
        for output in ('', '192.0.2.11 ssh-ed25519 ' + public,
                       '192.0.2.10 ssh-ed25519 ' + public + '\n192.0.2.10 ssh-ed25519 ' + base64.b64encode(b'different public key bytes').decode()):
            with self.subTest(output=output), patch.object(Path, 'is_file', return_value=True):
                with self.assertRaises((ValueError, RuntimeError)):
                    setup.keyscan(request(), run=lambda *a, **kw: SimpleNamespace(stdout=output))

    def test_fixed_builtin_names_match_current_platform(self):
        account = SimpleNamespace(pw_name='operator', pw_dir='/home/operator')
        for component, name in (('lower', 'rcd_server'), ('manager', 'RcdManager'), ('agent', 'RcdAgent'), ('communicate', 'rcd_communicate')):
            plan = setup.fixed_plan(dict(request(), component=component), account)
            self.assertEqual(plan['entry'], Path('/opt/rcd/bin') / name)
            self.assertIsNone(plan['unit'])
            self.assertIsNone(plan['profile'])
            self.assertIn('停止', plan['warning'])

    def test_generic_profile_has_project_identity_and_shared_host_lock(self):
        account = SimpleNamespace(pw_name='operator', pw_dir='/home/operator')
        plan = setup.fixed_plan(dict(request(), component='demo', application=application()), account)
        profile = plan['profile']
        self.assertEqual(profile['host_lock_dir'], '/opt/rcd/bin')
        self.assertEqual(profile['application']['project_id'], 1)
        self.assertEqual(profile['application']['definition_sha256'], 'b' * 64)
        self.assertEqual(plan['profile_path'], Path('/etc/rcd/release-platform/demo.json'))
        self.assertEqual(profile['control'], {'kind': 'systemd-user', 'unit': 'rcd-app-demo.service', 'run_as': 'operator'})
        self.assertIn('source_dir', profile)
        self.assertIn('release_dir', profile)

    def test_generic_definition_cannot_supply_paths_or_shell(self):
        account = SimpleNamespace(pw_name='operator', pw_dir='/home/operator')
        for update in ({'install_path': '/etc/passwd'}, {'service_unit': 'ssh.service'},
                       {'arguments': ['sh; reboot']}, {'service_user': 'root'}, {'graceful_shutdown_confirmed': False}):
            with self.subTest(update=update), self.assertRaises(ValueError):
                setup.fixed_plan(dict(request(), component='demo', application=dict(application(), **update)), account)

    def test_unit_is_non_shell_and_never_auto_restarts_or_kills(self):
        unit = setup.unit_text('/opt/rcd/apps/demo/bin/demo', '/var/lib/rcd/apps/demo', ['--name', 'two words'])
        self.assertIn('ExecStart="/opt/rcd/apps/demo/bin/demo" "--name" "two words"', unit)
        for value in ('Restart=no', 'SendSIGKILL=no', 'TimeoutStopSec=30s', 'KillSignal=SIGTERM', 'KillMode=control-group'):
            self.assertIn(value, unit)
        self.assertNotIn('ExecStartPre', unit)
        self.assertNotIn('ExecStop=', unit)
        self.assertIn('manual-stop.json', unit)

    def test_first_install_hold_is_consumed_by_explicit_application_start(self):
        # Exercise the actual consumer with the actual producer's JSON, but all
        # service/process actions are inert fixtures. No systemctl is invoked.
        from service_control import ServiceControl
        with tempfile.TemporaryDirectory() as directory:
            hold = Path(directory) / 'manual-stop.json'
            setup.new_file(hold, json.dumps(setup.first_install_hold(request())).encode(), 0o600)
            self.assertEqual(json.loads(hold.read_text())['desired_state'], 'stopped')
            calls = []
            operation = object.__new__(ServiceControl)
            operation.hold = hold
            operation.expected = 'c' * 64
            operation.r = SimpleNamespace(
                require_application_service=lambda: None,
                p={'control': {'unit': 'rcd-app-demo.service'}},
                systemctl=lambda *args: calls.append(args),
                accept=lambda digest: {'pid': 42, 'binary_sha256': digest})
            operation.d = SimpleNamespace(stopped=lambda: None, step=lambda name, action: action())
            operation.application_start()
            self.assertFalse(hold.exists())
            self.assertEqual(calls, [('enable', 'rcd-app-demo.service'), ('start', '--no-block', 'rcd-app-demo.service')])
            self.assertEqual(operation.acceptance['state'], 'running')

    def test_safe_publish_cannot_overwrite_or_follow_symlink(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            target = root / 'target'
            setup.new_file(target, b'first', 0o600)
            with self.assertRaises(FileExistsError):
                setup.new_file(target, b'other', 0o600)
            self.assertEqual(target.read_bytes(), b'first')
            (root / 'linked').symlink_to(root, target_is_directory=True)
            with self.assertRaises(OSError):
                setup.new_file(root / 'linked/escape', b'no', 0o600)
            self.assertFalse((root / 'escape').exists())

    def test_binary_publication_is_no_clobber(self):
        with tempfile.TemporaryDirectory() as directory:
            source = Path(directory) / 'source'
            source.write_bytes(b'inert bytes never executed')
            target = Path(directory) / 'binary'
            setup.new_binary(target, source)
            self.assertEqual(target.read_bytes(), source.read_bytes())
            self.assertEqual(target.stat().st_mode & 0o777, 0o755)
            with self.assertRaises(FileExistsError):
                setup.new_binary(target, source)

    def test_nonroot_install_fails_before_any_filesystem_write(self):
        with patch.object(setup.os, 'geteuid', return_value=1000), patch.object(setup, 'ensure_directory') as mkdir:
            with self.assertRaises(RuntimeError):
                setup.install(dict(request(), component='manager'), Path('/not-used'))
            mkdir.assert_not_called()

    def build_archive(self, root, member_names=('manifest.json', 'binary'), component='demo', adapter='1.1.0'):
        binary = bytearray(64)
        binary[:7] = b'\x7fELF\x02\x01\x01'
        binary[16:18] = (2).to_bytes(2, 'little')
        binary[18:20] = (62).to_bytes(2, 'little')
        digest = hashlib.sha256(binary).hexdigest()
        manifest = {'schema_version': 1, 'component': component, 'binary_sha256': digest,
                    'binary_size': len(binary), 'target': 'linux/amd64', 'compatibility': {'component_min_versions': {}, 'adapter_min_version': adapter},
                    'version': '1.0.0', 'release_id': 'fixture-first', 'commit': 'd' * 40,
                    'source_sha256': 'e' * 64, 'dirty': False, 'built_at': '2026-10-21T00:00:00Z'}
        archive = root / 'package.rcd.tar.gz'
        with tarfile.open(archive, 'w:gz', format=tarfile.USTAR_FORMAT) as output:
            for name in member_names:
                data = json.dumps(manifest).encode() if name == 'manifest.json' else bytes(binary)
                header = tarfile.TarInfo(name)
                header.size = len(data)
                output.addfile(header, io.BytesIO(data))
        req = dict(request(), component=component, binary_sha256=digest, archive_sha256=setup.digest(archive), package_id='d' * 64)
        return req, archive

    def test_extracts_exact_inert_elf_without_executing(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(setup.platform, 'machine', return_value='x86_64'):
            root = Path(directory)
            req, archive = self.build_archive(root)
            destination = root / 'stage'
            destination.mkdir()
            with patch.object(setup.subprocess, 'run', side_effect=AssertionError('binary execution forbidden')):
                binary, manifest = setup.extract_verified(req, archive, destination)
            self.assertEqual(setup.digest(binary), req['binary_sha256'])
            self.assertEqual(manifest['component'], 'demo')

    def test_rejects_duplicate_traversal_extra_and_wrong_architecture(self):
        for names in (('manifest.json', 'binary', 'binary'), ('manifest.json', '../binary'), ('manifest.json', 'binary', 'install.sh')):
            with self.subTest(names=names), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                req, archive = self.build_archive(root, names)
                stage = root / 'stage'
                stage.mkdir()
                with self.assertRaises(ValueError):
                    setup.extract_verified(req, archive, stage)
        with tempfile.TemporaryDirectory() as directory, patch.object(setup.platform, 'machine', return_value='aarch64'):
            root = Path(directory)
            req, archive = self.build_archive(root)
            stage = root / 'stage'; stage.mkdir()
            with self.assertRaises(ValueError):
                setup.extract_verified(req, archive, stage)

    def test_digest_changed_archive_rejected_without_extract(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            req, archive = self.build_archive(root)
            archive.write_bytes(archive.read_bytes() + b'corruption')
            stage = root / 'stage'; stage.mkdir()
            with self.assertRaises(ValueError):
                setup.extract_verified(req, archive, stage)
            self.assertEqual(list(stage.iterdir()), [])

    def test_newer_adapter_requirement_is_not_ignored_for_first_install(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(setup.platform, 'machine', return_value='x86_64'):
            root = Path(directory)
            req, archive = self.build_archive(root, adapter='9.0.0')
            stage = root / 'stage'; stage.mkdir()
            with self.assertRaisesRegex(ValueError, 'adapter version'):
                setup.extract_verified(req, archive, stage)

    def fixture_install(self, root):
        """Map EVERY target path into tmp; no native service/lock/proc calls."""
        stack = ExitStack()
        original_path = Path
        def mapped(value):
            value = str(value)
            for prefix in ('/opt/rcd', '/var/lib/rcd', '/var/lib/rcd-host-setup',
                           '/var/lib/rcd-release-platform', '/etc/rcd', '/home/operator'):
                if value == prefix or value.startswith(prefix + '/'):
                    return root / value.lstrip('/')
            return original_path(value)
        account = SimpleNamespace(pw_name='operator', pw_dir='/home/operator', pw_uid=os.getuid(), pw_gid=os.getgid())
        def require_fixture(value, dir_fd=None):
            if isinstance(value, int):
                candidate = original_path(os.readlink('/proc/self/fd/' + str(value)))
            else:
                candidate = original_path(value)
                if not candidate.is_absolute():
                    base = original_path(os.readlink('/proc/self/fd/' + str(dir_fd))) if dir_fd is not None else original_path.cwd()
                    candidate = base / candidate
            try:
                candidate.resolve().relative_to(root.resolve())
            except ValueError:
                raise AssertionError('fixture attempted a mutation outside its temporary root: ' + str(candidate))
        original_open, original_mkdir = os.open, os.mkdir
        original_unlink, original_rmdir = os.unlink, os.rmdir
        original_replace, original_chmod = os.replace, os.chmod
        original_fchmod, original_fchown = os.fchmod, os.fchown
        original_io_open = io.open
        original_temporary_directory = tempfile.TemporaryDirectory
        def guarded_open(path, flags, mode=0o777, *, dir_fd=None):
            if flags & (os.O_WRONLY | os.O_RDWR | os.O_CREAT | os.O_TRUNC | os.O_APPEND):
                require_fixture(path, dir_fd)
            return original_open(path, flags, mode, dir_fd=dir_fd)
        def guarded_mkdir(path, mode=0o777, *, dir_fd=None):
            require_fixture(path, dir_fd)
            return original_mkdir(path, mode, dir_fd=dir_fd)
        def guarded_unlink(path, *, dir_fd=None):
            require_fixture(path, dir_fd)
            return original_unlink(path, dir_fd=dir_fd)
        def guarded_rmdir(path, *, dir_fd=None):
            require_fixture(path, dir_fd)
            return original_rmdir(path, dir_fd=dir_fd)
        def guarded_replace(source, target, *, src_dir_fd=None, dst_dir_fd=None):
            require_fixture(source, src_dir_fd); require_fixture(target, dst_dir_fd)
            return original_replace(source, target, src_dir_fd=src_dir_fd, dst_dir_fd=dst_dir_fd)
        def guarded_chmod(path, mode, *, dir_fd=None, follow_symlinks=True):
            require_fixture(path, dir_fd)
            return original_chmod(path, mode, dir_fd=dir_fd, follow_symlinks=follow_symlinks)
        def guarded_fchmod(fd, mode):
            require_fixture(fd)
            return original_fchmod(fd, mode)
        def guarded_fchown(fd, uid, gid):
            require_fixture(fd)
            return original_fchown(fd, uid, gid)
        def guarded_io_open(file, mode='r', *args, **kwargs):
            if any(flag in mode for flag in 'wax+'):
                require_fixture(file)
            return original_io_open(file, mode, *args, **kwargs)
        def fixture_temporary_directory(*args, **kwargs):
            if kwargs.get('dir') is not None:
                kwargs['dir'] = str(mapped(kwargs['dir']))
            return original_temporary_directory(*args, **kwargs)
        for name, replacement in (('open', guarded_open), ('mkdir', guarded_mkdir),
                                  ('unlink', guarded_unlink), ('remove', guarded_unlink),
                                  ('rmdir', guarded_rmdir), ('replace', guarded_replace),
                                  ('chmod', guarded_chmod), ('fchmod', guarded_fchmod), ('fchown', guarded_fchown)):
            stack.enter_context(patch.object(setup.os, name, side_effect=replacement))
        stack.enter_context(patch.object(io, 'open', side_effect=guarded_io_open))
        stack.enter_context(patch.object(setup.tempfile, 'TemporaryDirectory', side_effect=fixture_temporary_directory))
        stack.enter_context(patch.object(setup, 'Path', side_effect=mapped))
        # This unprivileged tmp fixture models trusted root-owned system
        # ancestors. Dedicated permission tests below exercise the real guard;
        # all mutating syscalls remain independently restricted to tmp.
        stack.enter_context(patch.object(setup, 'require_system_directory'))
        stack.enter_context(patch.object(setup.os, 'geteuid', return_value=0))
        stack.enter_context(patch.object(setup, 'probe', return_value={}))
        stack.enter_context(patch.object(setup.pwd, 'getpwnam', return_value=account))
        stack.enter_context(patch.object(setup, 'target_lock', side_effect=lambda release: nullcontext()))
        stack.enter_context(patch.object(setup, 'require_no_unfinished_release'))
        stack.enter_context(patch.object(setup, 'require_no_existing_process'))
        stack.enter_context(patch.object(setup.platform, 'machine', return_value='x86_64'))
        stack.enter_context(patch.object(setup.subprocess, 'run', side_effect=AssertionError('no executable or systemctl may run')))
        return stack

    def test_install_fixture_refuses_any_mutation_outside_temporary_root(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            with self.fixture_install(root):
                for target in ('/var/lib/rcd-host-setup', '/opt/rcd/bin', '/etc/rcd'):
                    with self.assertRaisesRegex(AssertionError, 'outside its temporary root'):
                        setup.os.mkdir(target)
                with self.assertRaisesRegex(AssertionError, 'outside its temporary root'):
                    io.open('/var/lib/rcd-host-setup/must-not-be-created', 'w')

    def test_complete_first_install_into_fixture_preserves_stopped_hold_and_identity(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            req, archive = self.build_archive(root)
            req['application'] = application()
            with self.fixture_install(root):
                report = setup.install(req, archive)
            self.assertEqual(report['state'], 'installed_stopped')
            self.assertFalse(report['business_ready'])
            entry = root / 'opt/rcd/apps/demo/bin/demo'
            self.assertEqual(setup.digest(entry), req['binary_sha256'])
            runtime = root / 'var/lib/rcd/apps/demo'
            self.assertEqual(json.loads((runtime / 'manual-stop.json').read_text())['desired_state'], 'stopped')
            self.assertEqual(json.loads((runtime / '.rcd-application-owner.json').read_text())['project_id'], 1)
            unit = root / 'home/operator/.config/systemd/user/rcd-app-demo.service'
            self.assertIn('Restart=no', unit.read_text())
            self.assertFalse((unit.parent / 'default.target.wants/rcd-app-demo.service').exists())
            identity = root / 'var/lib/rcd-release-platform/artifacts/demo' / ('first-install-' + req['operation_id']) / 'identity-public.json'
            self.assertEqual(json.loads(identity.read_text())['components']['demo']['sha256'], req['binary_sha256'])

    def test_rejection_before_intent_is_known_no_install_and_keeps_existing_file(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            req, archive = self.build_archive(root)
            req['application'] = application()
            entry = root / 'opt/rcd/apps/demo/bin/demo'
            entry.parent.mkdir(parents=True)
            entry.write_bytes(b'existing unowned program')
            with self.fixture_install(root), self.assertRaises(setup.InstallNotStarted):
                setup.install(req, archive)
            self.assertEqual(entry.read_bytes(), b'existing unowned program')
            self.assertFalse((root / 'var/lib/rcd-host-setup' / (req['operation_id'] + '.json')).exists())
            self.assertFalse((root / 'opt/rcd/bin/.rcd-host-release-active.json').exists())

    def test_failure_after_intent_retains_unknown_and_reconcile_never_reinstalls(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            req, archive = self.build_archive(root)
            req['application'] = application()
            original = setup.new_binary
            def partial(path, source):
                original(path, source)
                raise OSError('fixture interruption after writing binary')
            with self.fixture_install(root), patch.object(setup, 'new_binary', side_effect=partial):
                with self.assertRaises(OSError) as caught:
                    setup.install(req, archive)
                self.assertNotIsInstance(caught.exception, setup.InstallNotStarted)
            receipt = root / 'var/lib/rcd-host-setup' / (req['operation_id'] + '.json')
            self.assertEqual(json.loads(receipt.read_text())['report']['state'], 'unknown')
            with self.fixture_install(root), patch.object(setup, 'new_binary', side_effect=AssertionError('must not replay')):
                report = setup.install(req, archive)
            self.assertEqual(report['state'], 'unknown')

    def test_existing_intent_precedes_changed_privilege_and_prerequisites(self):
        for condition in ('nonroot', 'account_missing', 'probe_failed'):
            with self.subTest(condition=condition), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                req, archive = self.build_archive(root)
                req['application'] = application()
                journal = root / 'var/lib/rcd-host-setup' / (req['operation_id'] + '.json')
                journal.parent.mkdir(parents=True)
                report = dict(setup.base_report(req, 'install'), state='unknown', error='original partial install')
                journal.write_text(json.dumps({'schema_version': 1, 'request': req, 'report': report}))
                with self.fixture_install(root), patch.object(setup.os, 'geteuid', return_value=1000), \
                        patch.object(setup, 'probe', side_effect=RuntimeError('probe now fails')), \
                        patch.object(setup.pwd, 'getpwnam', side_effect=KeyError('account gone')), \
                        patch.object(setup, 'new_binary', side_effect=AssertionError('no replay')):
                    self.assertEqual(setup.install(req, archive), report)

    def test_unfinished_marker_states_are_not_fresh_install_failures(self):
        for path, state in (('opt/rcd/bin/.rcd-host-release-active.json', 'failed'),
                            ('opt/rcd/apps/demo/bin/.demo.release-active.json', 'recovered'),
                            ('opt/rcd/apps/demo/bin/.demo.release-active.json', 'failed')):
            with self.subTest(path=path, state=state), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                req, archive = self.build_archive(root)
                req['application'] = application()
                marker = root / path
                marker.parent.mkdir(parents=True)
                marker.write_text(json.dumps({'state': state}))
                with self.fixture_install(root), patch.object(setup.os, 'geteuid', return_value=1000):
                    with self.assertRaises(RuntimeError) as error:
                        setup.install(req, archive)
                    self.assertNotIsInstance(error.exception, setup.InstallNotStarted)

    def test_corrupt_prior_journal_cannot_release_unknown_reservation(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            req, archive = self.build_archive(root)
            journal = root / 'var/lib/rcd-host-setup' / (req['operation_id'] + '.json')
            journal.parent.mkdir(parents=True)
            journal.write_text('{partial')
            with self.fixture_install(root), patch.object(setup.os, 'geteuid', return_value=1000):
                with self.assertRaises(ValueError) as error:
                    setup.install(req, archive)
                self.assertNotIsInstance(error.exception, setup.InstallNotStarted)

    def test_read_only_reconcile_proves_original_without_package_or_mutation(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            req, archive = self.build_archive(root)
            req['application'] = application()
            with self.fixture_install(root):
                setup.install(req, archive)
            paths = [p for p in root.rglob('*') if p.is_file()]
            before = {str(p): p.read_bytes() for p in paths}
            query = dict(req, task_id=99, original_task_id=req['task_id'])
            with self.fixture_install(root), patch.object(setup, 'read_only_host_lock', side_effect=lambda: nullcontext()), \
                    patch.object(setup, 'new_file', side_effect=AssertionError('query must not write')), \
                    patch.object(setup, 'new_binary', side_effect=AssertionError('query must not install')), \
                    patch.object(setup, 'extract_verified', side_effect=AssertionError('query needs no archive')):
                report = setup.reconcile(query)
            self.assertEqual(report['state'], 'installed_stopped')
            self.assertEqual(report['phase'], 'reconcile')
            self.assertEqual(report['task_id'], 99)
            self.assertEqual(report['original_report']['task_id'], req['task_id'])
            self.assertEqual(before, {str(p): p.read_bytes() for p in paths})

    def test_read_only_reconcile_rejects_wrong_original_or_changed_binary_or_marker(self):
        for changed in ('task', 'binary', 'marker', 'hold', 'admission'):
            with self.subTest(changed=changed), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                req, archive = self.build_archive(root)
                req['application'] = application()
                req['admission_id'] = 'f' * 64
                with self.fixture_install(root):
                    setup.install(req, archive)
                query = dict(req, task_id=99, original_task_id=req['task_id'])
                if changed == 'task':
                    query['original_task_id'] = 11
                elif changed == 'admission':
                    query['admission_id'] = 'e' * 64
                elif changed == 'binary':
                    (root / 'opt/rcd/apps/demo/bin/demo').write_bytes(b'changed')
                elif changed == 'marker':
                    (root / 'opt/rcd/bin/.rcd-host-release-active.json').write_text(json.dumps({'state': 'running'}))
                else:
                    (root / 'var/lib/rcd/apps/demo/manual-stop.json').unlink()
                with self.fixture_install(root), patch.object(setup, 'read_only_host_lock', side_effect=lambda: nullcontext()):
                    with self.assertRaises((RuntimeError, ValueError)):
                        setup.reconcile(query)

    def test_read_only_lock_does_not_create_missing_file(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'opt/rcd/bin').mkdir(parents=True)
            with self.fixture_install(root), self.assertRaises(FileNotFoundError):
                with setup.read_only_host_lock():
                    self.fail('must not manufacture an installation lock')
            self.assertFalse((root / 'opt/rcd/bin/.rcd-host-release.lock').exists())

    def test_read_only_lock_refuses_nonregular_inode_without_blocking(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            lock = root / 'opt/rcd/bin/.rcd-host-release.lock'
            lock.parent.mkdir(parents=True)
            os.mkfifo(lock)
            with self.fixture_install(root), self.assertRaisesRegex(RuntimeError, 'regular non-linked'):
                with setup.read_only_host_lock():
                    self.fail('a FIFO must never be accepted as a host lock')

    def test_system_parent_wrong_owner_or_writable_permissions_rejected_without_repair(self):
        for uid, mode in ((1000, 0o755), (0, 0o775), (0, 0o757)):
            with self.subTest(uid=uid, mode=oct(mode)), tempfile.TemporaryDirectory() as directory:
                parent = Path(directory) / 'controlled-system'
                parent.mkdir()
                before = parent.stat()
                def metadata(fd):
                    path = Path(os.readlink('/proc/self/fd/' + str(fd)))
                    return SimpleNamespace(st_uid=uid if path == parent else 0,
                                           st_mode=stat.S_IFDIR | (mode if path == parent else 0o755))
                with patch.object(setup.os, 'fstat', side_effect=metadata), \
                        patch.object(setup.os, 'mkdir') as mkdir, \
                        patch.object(setup.os, 'chmod') as chmod, \
                        patch.object(setup.os, 'fchmod') as fchmod, \
                        patch.object(setup.os, 'fchown') as fchown:
                    with self.assertRaisesRegex(RuntimeError, 'root-owned'):
                        setup.ensure_directory(parent / 'must-not-be-created')
                    for mutation in (mkdir, chmod, fchmod, fchown):
                        mutation.assert_not_called()
                self.assertFalse((parent / 'must-not-be-created').exists())
                self.assertEqual((parent.stat().st_uid, parent.stat().st_mode), (before.st_uid, before.st_mode))

    def test_service_owned_data_directory_retains_existing_permission_contract(self):
        with tempfile.TemporaryDirectory() as directory:
            parent = Path(directory)
            owner = SimpleNamespace(pw_uid=os.getuid(), pw_gid=os.getgid())
            before = parent.stat()
            with patch.object(setup, 'require_system_directory', side_effect=AssertionError('business directory is not a root-owned system directory')):
                setup.ensure_directory(parent, 0o750, owner)
            self.assertEqual((parent.stat().st_uid, parent.stat().st_mode), (before.st_uid, before.st_mode))


if __name__ == '__main__':
    unittest.main()
