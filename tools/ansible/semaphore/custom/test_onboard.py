import unittest
from onboard import inventory_hosts, register


class API:
    def __init__(self): self.rows = {}; self.next = 1; self.calls = []
    def request(self, path, data=None, method=None, headers=None):
        self.calls.append((path, data))
        if method == 'PUT' and path.endswith('/control-binding'):
            device_id = int(path.split('/')[-2])
            row = next(d for d in self.rows['/project/1/rcd/devices'] if d['id'] == device_id)
            if headers['If-Match'] != '"' + str(row['revision']) + '"':
                raise AssertionError('revision header mismatch')
            row.update(control_template_id=data['control_template_id'], revision=row['revision'] + 1)
            return row
        if '/rcd/devices?' in path:
            return {'items': self.rows.get('/project/1/rcd/devices', [])}
        if data is None: return self.rows.get(path, [])
        item = dict(data, id=self.next); self.next += 1
        if path.endswith('/rcd/devices'): item['revision'] = 1
        self.rows.setdefault(path, []).append(item)
        return item


class OnboardingTests(unittest.TestCase):
    def test_shared_inventory_serial_targets_without_credentials(self):
        hosts = inventory_hosts()
        self.assertEqual(list(hosts), ['rcd30','rcd58','rcd46'])
        for h in hosts.values():
            self.assertNotIn('ansible_password', h)
            self.assertEqual(len(h['rcd_services']), 2)
            self.assertTrue(h['rcd_inspection_become'])
            self.assertEqual(set(h['rcd_release_profiles']), {'lower','manager','agent'})

    def test_repeated_setup_creates_no_tasks_or_duplicate_schedules(self):
        api=API()
        a=register(api,1,'/fixed/snapshot','private',True)
        b=register(api,1,'/fixed/snapshot','private',True)
        self.assertEqual(a,b)
        self.assertEqual(len(api.rows['/project/1/schedules']),3)
        self.assertFalse(any('/tasks' in path or '/operations' in path for path,_ in api.calls))

    def test_existing_binding_drift_is_not_overwritten(self):
        api=API();register(api,1,'/fixed/snapshot','private',False)
        with self.assertRaisesRegex(RuntimeError,'differs'):
            register(api,1,'/different/snapshot','private',False)

    def test_service_control_is_opt_in_and_does_not_execute(self):
        api = API()
        register(api,1,'/fixed/snapshot','private',False)
        self.assertFalse(any(t.get('playbook','').endswith('control-platform.yml') for t in api.rows['/project/1/templates']))
        register(api,1,'/fixed/snapshot','private',False,True)
        register(api,1,'/fixed/snapshot','private',False,True)
        self.assertEqual(len([p for p,_ in api.calls if p.endswith('/control-binding')]),3)
        self.assertFalse(any('/tasks' in p or '/operations' in p for p,_ in api.calls))
        self.assertTrue(all(d.get('control_template_id') for d in api.rows['/project/1/rcd/devices']))


if __name__=='__main__':unittest.main()
