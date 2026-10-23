from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import refresh_snapshot


class API:
    def __init__(self, second_state):self.second_state=second_state;self.calls=[]
    def request(self,path,data=None,method=None):
        self.calls.append((path,data,method))
        if '/rcd/operations?' in path:
            return {'items':[{'state':self.second_state if path.endswith('page=2') else 'succeeded'}],'total_pages':2}
        if path.endswith('/rcd/host-setups'):return {'items':[]}
        if data is None:return {'git_url':'/previous/snapshot'}
        return data


class SnapshotTests(unittest.TestCase):
    def test_active_host_setup_blocks_tools_update(self):
        api=API('succeeded');original=api.request
        def request(path,*args,**kwargs):
            if path.endswith('/rcd/host-setups'):return {'items':[{'state':'unknown'}]}
            return original(path,*args,**kwargs)
        with patch.object(api,'request',side_effect=request),patch.object(refresh_snapshot.os,'geteuid',return_value=0),patch.object(refresh_snapshot.subprocess,'check_output',return_value='192.168.10.46'):
            with self.assertRaisesRegex(RuntimeError,'active host setup'):
                refresh_snapshot.publish(Path('/unused/source'),1,2,api)
    def test_unresolved_operation_on_second_page_blocks_update(self):
        api=API('unknown')
        with patch.object(refresh_snapshot.os,'geteuid',return_value=0),patch.object(refresh_snapshot.subprocess,'check_output',return_value='192.168.10.46'):
            with self.assertRaisesRegex(RuntimeError,'active publication'):
                refresh_snapshot.publish(Path('/unused/source'),1,2,api)
        self.assertEqual(len(api.calls),2)

    def test_verified_snapshot_uses_all_pages_and_keeps_old_tree(self):
        for terminal in ('succeeded','failed_before_execution','partial_failed'):
            with self.subTest(state=terminal):
                self.assert_terminal_allows_snapshot(terminal)

    def assert_terminal_allows_snapshot(self, terminal):
        api=API(terminal)
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);source=root/'source';destination=root/'snapshots';destination.mkdir()
            for folder in ['release','ansible']:
                p=source/'tools'/folder;p.mkdir(parents=True);(p/'example.py').write_text('public tool source')
            with patch.object(refresh_snapshot.os,'geteuid',return_value=0),patch.object(refresh_snapshot.subprocess,'check_output',return_value='192.168.10.46'),patch.object(refresh_snapshot,'Path',return_value=destination):
                refresh_snapshot.publish(source,1,2,api)
            self.assertEqual(len(list(destination.glob('tools-*/manifest.json'))),1)
            self.assertEqual(api.calls[-1][2],'PUT')


if __name__=='__main__':unittest.main()
