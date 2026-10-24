import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import import_catalog


class Uploads:
    def __init__(self):
        self.requests = []

    def open(self, request, timeout):
        self.requests.append(request)
        return io.BytesIO(json.dumps({'id': str(len(self.requests)), 'revision': 1, 'enabled': True}).encode())


class API:
    base = 'http://127.0.0.1:3000'

    def __init__(self):
        self.http = Uploads()
        self.disabled = []

    def request(self, path, data, method, headers):
        self.disabled.append((path, data, method, headers))
        return dict(data, id=path.split('/')[-2])


class LegacyImportTests(unittest.TestCase):
    def test_legacy_packages_are_archival_and_disabled_without_deployment(self):
        api = API()
        exports = []

        def export(build, sha, compatibility, notes, output):
            exports.append((build.name, compatibility, notes))
            output.write_bytes(b'validated-archive')

        with tempfile.TemporaryDirectory() as tmp:
            with patch.object(import_catalog, 'export', export), patch.object(import_catalog, 'digest', return_value='a' * 64):
                import_catalog.import_catalog(api, 1, Path(tmp), Path(tmp))
        self.assertEqual([row[0] for row in exports], ['lower', 'manager', 'agent'])
        self.assertTrue(all(row[1]['adapter_min_version'] == '999.0.0' for row in exports))
        self.assertTrue(all('仅归档' in row[2] for row in exports))
        self.assertEqual(len(api.disabled), 3)
        for path, data, method, headers in api.disabled:
            self.assertTrue(path.endswith('/enabled'))
            self.assertFalse(data['enabled'])
            self.assertEqual(method, 'PUT')
            self.assertEqual(headers['If-Match'], '"1"')
        self.assertTrue(all(r.full_url.endswith('/rcd/packages') for r in api.http.requests))

    def test_invalid_legacy_catalog_does_not_upload(self):
        api = API()
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaises(FileNotFoundError):
                import_catalog.import_catalog(api, 1, Path(tmp), Path(tmp))
        self.assertEqual(api.http.requests, [])


if __name__ == '__main__':
    unittest.main()
