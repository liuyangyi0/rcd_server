import copy
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from application_profile import application_slug, validate_application_profile, valid_arguments, unit_text
from release import Release


class ApplicationProfileTests(unittest.TestCase):
    def profile(self):
        return {'host': '192.0.2.1', 'component': 'my-app',
                'executable': '/opt/rcd/apps/my-app/bin/my-app',
                'runtime_dir': '/var/lib/rcd/apps/my-app', 'work_dir': '/var/lib/rcd/apps/my-app',
                'host_lock_dir': '/opt/rcd/bin', 'databases': [],
                'control': {'kind': 'systemd-user', 'run_as': 'operator', 'unit': 'rcd-app-my-app.service'},
                'application': {'schema_version': 1, 'project_id': 1, 'component': 'my-app',
                                'definition_sha256': 'a'*64, 'arguments': [], 'graceful_shutdown_confirmed': True}}

    def test_generated_paths_and_account_are_required(self):
        profile = self.profile()
        validate_application_profile(profile, check_owner=False)
        for key, value in [('executable', '/opt/rcd/bin/rcd_server'),
                           ('runtime_dir', '/var/lib/rcd/lower'), ('host_lock_dir', '/tmp'),
                           ('databases', ['/some/database']), ('health_url', 'http://127.0.0.1:8087')]:
            with self.subTest(key=key), self.assertRaises(ValueError):
                validate_application_profile(dict(profile, **{key:value}), check_owner=False)
        for key,value in [('run_as', 'root'), ('unit','rcd-lower.service'), ('kind','process-agent')]:
            changed = copy.deepcopy(profile); changed['control'][key]=value
            with self.subTest(key=key), self.assertRaises(ValueError):
                validate_application_profile(changed, check_owner=False)

    def test_reserved_components_and_shell_arguments_cannot_be_apps(self):
        for slug in ('manager','lower','agent','communicate','lower-stack','rcd-server','systemd','../x','TEST','9app','app-'):
            self.assertFalse(application_slug(slug), slug)
        for args in (['$(id)'], ['$HOME'], ['%h'], ['x;y'], ['../file'], ['a\nb'], None, ['x']*17):
            self.assertFalse(valid_arguments(args), args)
        self.assertTrue(valid_arguments(['--listen=127.0.0.1:9000', 'two words']))

    def test_missing_ownership_never_assumes_project_identity(self):
        with patch.object(Path, 'is_file', return_value=False), self.assertRaisesRegex(ValueError, 'ownership'):
            validate_application_profile(self.profile())

    def test_unit_has_no_scripts_expansions_or_forced_kill(self):
        text = unit_text('/opt/rcd/apps/my-app/bin/my-app', '/var/lib/rcd/apps/my-app', ['--mode=slow', 'two words'])
        self.assertIn('SendSIGKILL=no', text)
        self.assertIn('Restart=no', text)
        self.assertIn('"--mode=slow" "two words"', text)
        self.assertNotIn('ExecStop=', text)
        for args in (['%h'], ['$HOME'], ['a\nb']):
            with self.assertRaises(ValueError):
                unit_text('/fixed', '/fixed-data', args)

    def test_full_unit_verification_rejects_extra_hooks_or_dropins(self):
        r = object.__new__(Release)
        r.p = self.profile(); r.entry=Path(r.p['executable']); r.runtime=Path(r.p['runtime_dir'])
        with tempfile.TemporaryDirectory() as directory:
            fragment=Path(directory)/'rcd-app-my-app.service'
            expected=unit_text(str(r.entry),str(r.runtime),[])
            fragment.write_text(expected)
            values={'LoadState':'loaded','MainPID':'9','Job':'','SendSIGKILL':'no','KillSignal':'15',
                    'KillMode':'control-group','Restart':'no','TimeoutStopUSec':'30s',
                    'WorkingDirectory':str(r.runtime),'ExecStart':'{ path='+str(r.entry)+' ; argv[]='+str(r.entry)+' ; }',
                    'FragmentPath':str(fragment),'DropInPaths':''}
            r.systemctl=lambda *args:'\n'.join(k+'='+v for k,v in values.items())
            with patch('release.validate_application_profile'):
                r.require_application_service()
                fragment.write_text(expected+'\n[Service]\nExecStartPre=/bin/true\n')
                with self.assertRaisesRegex(RuntimeError, 'hooks'):
                    r.require_application_service()
                fragment.write_text(expected); values['DropInPaths']='/tmp/override.conf'
                with self.assertRaisesRegex(RuntimeError, 'drop-ins'):
                    r.require_application_service()


if __name__ == '__main__':
    unittest.main()
