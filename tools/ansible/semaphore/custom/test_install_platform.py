import os
from pathlib import Path
import sqlite3
import tempfile
import unittest

from install_platform import restore_database, idle


class PlatformRecoveryTests(unittest.TestCase):
    def test_pending_first_install_blocks_platform_restart(self):
        with tempfile.TemporaryDirectory() as temp:
            path=Path(temp)/'platform.sqlite'
            with sqlite3.connect(path) as db:
                db.execute('create table task(status text)')
                db.execute('create table rcd_host_setup_active(setup_id text)')
                db.execute("insert into rcd_host_setup_active values('unknown')")
            with self.assertRaisesRegex(RuntimeError,'host setup'):idle(path)
    def test_backup_failure_before_start_keeps_only_live_database(self):
        with tempfile.TemporaryDirectory() as temp:
            root=Path(temp);db=root/'live.sqlite';backup=root/'backup';backup.mkdir()
            db.write_bytes(b'original untouched');(backup/'semaphore.sqlite').write_bytes(b'incomplete')
            restore_database(db,backup,False,os.getuid(),os.getgid())
            self.assertEqual(db.read_bytes(),b'original untouched')
            self.assertFalse((backup/'failed-database').exists())

    def test_corrupt_restore_backup_is_checked_before_moving_current_db(self):
        with tempfile.TemporaryDirectory() as temp:
            root=Path(temp);db=root/'live.sqlite';backup=root/'backup';backup.mkdir()
            db.write_bytes(b'current');(backup/'semaphore.sqlite').write_bytes(b'corrupt')
            with self.assertRaises(sqlite3.DatabaseError):
                restore_database(db,backup,True,os.getuid(),os.getgid())
            self.assertEqual(db.read_bytes(),b'current')

    def test_after_migration_restore_matching_database_and_keep_failed_copy(self):
        with tempfile.TemporaryDirectory() as temp:
            root=Path(temp);db=root/'live.sqlite';backup=root/'backup';backup.mkdir()
            with sqlite3.connect(backup/'semaphore.sqlite') as c:c.execute('pragma user_version=1')
            with sqlite3.connect(db) as c:c.execute('pragma user_version=2')
            restore_database(db,backup,True,os.getuid(),os.getgid())
            with sqlite3.connect(db) as c:self.assertEqual(c.execute('pragma user_version').fetchone()[0],1)
            with sqlite3.connect(backup/'failed-database') as c:self.assertEqual(c.execute('pragma user_version').fetchone()[0],2)


if __name__=='__main__':unittest.main()
