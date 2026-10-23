import errno
import json
import hashlib
from pathlib import Path
import sqlite3
import tempfile
import unittest
from unittest.mock import patch
from contextlib import closing

from release_probe import probe, public_error, ReleaseProblem, read_formats


class ReleaseProbeTests(unittest.TestCase):
    def test_application_compatibility_rejects_another_project(self):
        profile={'component':'sample-app','host':'192.0.2.1','application':{'project_id':2,'definition_sha256':'a'*64}}
        original=__import__('release_probe').read_json
        def read(path,*args):
            if str(path).endswith('/sample-app.json'):return profile
            return original(path,*args)
        with patch('release_probe.read_json',side_effect=read),patch('release_probe.validate_application_profile'):
            result=probe(self.profiles,'192.0.2.1',applications=[{'component':'sample-app','definition_sha256':'a'*64}],project_id=1)
        self.assertIn('另一个项目',result['compatibility']['sample-app']['error'])
        self.assertEqual(result['compatibility']['manager']['state_formats'],{'manager_db':3})
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(); self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.profiles = {}
        for component in ('manager', 'lower', 'agent'):
            directory = self.root / component; directory.mkdir()
            profile = {'component': component, 'host': '192.0.2.1', 'runtime_dir': str(directory),
                       'release_dir': str(directory), 'databases': []}
            if component == 'agent':
                (directory/'process-state.json').write_text(json.dumps({'schema_version': 3}))
            else:
                names = ['manager.sqlite'] if component == 'manager' else ['command-journal.sqlite', 'maintenance.sqlite']
                for name in names:
                    db = directory / name
                    with closing(sqlite3.connect(db)) as c: c.execute('pragma user_version=3')
                    profile['databases'].append(str(db))
            path = directory/'profile.json'; path.write_text(json.dumps(profile))
            self.profiles[component] = str(path)

    def test_formats_and_missing_binding_fail_closed(self):
        result = probe(self.profiles, '192.0.2.1')
        self.assertEqual(result['compatibility']['manager']['state_formats'], {'manager_db': 3})
        bad = probe(self.profiles, '192.0.2.2')
        self.assertTrue(all(v['error'] for v in bad['compatibility'].values()))
        self.assertTrue(probe({}, '192.0.2.1')['compatibility']['agent']['error'])

    def test_machine_identity_is_read_from_fixed_database_not_browser(self):
        profile=json.loads(Path(self.profiles['manager']).read_text())
        with closing(sqlite3.connect(profile['databases'][0])) as db:
            db.execute('CREATE TABLE manager_identity(id INTEGER PRIMARY KEY,instance_id TEXT)')
            db.execute("INSERT INTO manager_identity VALUES(1,'stable-manager')");db.commit()
        result=probe(self.profiles,'192.0.2.1')
        self.assertEqual(result['compatibility']['manager']['instance_id'],'stable-manager')

    def test_fetch_lost_report_is_read_only_and_identity_bound(self):
        op = dict(operation_id='a'*64, task_id=12, device_id='device', host_alias='test', component='manager')
        self.assertIsNone(probe(self.profiles, '192.0.2.1', op)['release_report'])
        reports = self.root/'manager/platform-reports'; reports.mkdir()
        path = reports/(op['operation_id']+'.json')
        report = dict(op, schema_version=1, state='succeeded')
        path.write_text(json.dumps(report)); before = path.read_bytes()
        self.assertEqual(probe(self.profiles, '192.0.2.1', op)['release_report']['state'], 'succeeded')
        self.assertEqual(path.read_bytes(), before)
        self.assertIsNone(probe(self.profiles, '192.0.2.1', dict(op, task_id=13))['release_report'])
        self.assertIsNone(probe(self.profiles, '192.0.2.1', dict(op, operation_id='../escape'))['release_report'])

    def test_safe_actionable_errors_never_leak_exception_contents(self):
        for number, code in [(errno.ENOSPC,'disk_full'), (errno.EACCES,'permission_denied'), (errno.EROFS,'read_only_storage')]:
            error = public_error(OSError(number, 'secret-password'))
            self.assertEqual(error['code'], code); self.assertNotIn('secret', error['message'])
        self.assertIn('备份', public_error(RuntimeError('secret'), 'backed_up')['message'])
        self.assertEqual(public_error(ReleaseProblem('known','安全原因'))['message'], '安全原因')

    def test_incomplete_database_coverage_rejected(self):
        profile = json.loads(Path(self.profiles['lower']).read_text()); profile['databases'].pop()
        with self.assertRaises(ReleaseProblem): read_formats(profile)

    def test_missing_final_report_requires_completed_bound_journal_and_host_marker(self):
        op=dict(operation_id='e'*64, task_id=7, device_id='device', host_alias='test', component='manager')
        profile_path=Path(self.profiles['manager']);profile=json.loads(profile_path.read_text())
        profile['executable']=str(self.root/'manager/bin/manager');profile_path.write_text(json.dumps(profile))
        record={'schema_version':1,'deployment_id':op['operation_id'],'host':'192.0.2.1','component':'manager',
                'profile_sha256':hashlib.sha256(json.dumps(profile,sort_keys=True,separators=(',',':')).encode()).hexdigest(),
                'report_binding':{k:v for k,v in op.items() if k!='component'},'state':'succeeded','binary_sha256':'f'*64,
                'stages':[{'stage':'software_acceptance','state':'confirmed','result':{'binary_sha256':'f'*64}},
                          {'stage':'maintenance_completed','state':'confirmed'}]}
        journal=self.root/'manager/deployments'/op['operation_id']/'deployment.json';journal.parent.mkdir(parents=True)
        journal.write_text(json.dumps(record))
        marker=self.root/'manager/bin/.rcd-host-release-active.json';marker.parent.mkdir()
        marker.write_text(json.dumps({'deployment_id':op['operation_id'],'state':'running'}))
        self.assertIsNone(probe(self.profiles,'192.0.2.1',op)['release_report'])
        marker.write_text(json.dumps({'deployment_id':op['operation_id'],'state':'succeeded'}))
        report=probe(self.profiles,'192.0.2.1',op)['release_report']
        self.assertEqual(report['state'],'succeeded');self.assertNotIn('result',report['stages'][0])
        self.assertFalse((self.root/'manager/platform-reports').exists(),'readback must not create report files')
        record['stages'][-1]['state']='intent';journal.write_text(json.dumps(record))
        self.assertIsNone(probe(self.profiles,'192.0.2.1',op)['release_report'])


if __name__ == '__main__': unittest.main()
