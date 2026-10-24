import csv
import importlib.util
import io
from pathlib import Path
from types import SimpleNamespace
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('qemu_lab', Path(__file__).with_name('qemu-lab.py'))
lab = importlib.util.module_from_spec(spec)
spec.loader.exec_module(lab)


class ReplicationMigrationTests(unittest.TestCase):
    def test_refuses_replication_change_without_three_live_nodes(self):
        for status in (b'id,is_live\n1,true\n2,true\n', b'id,is_live\n1,true\n2,true\n3,false\n'):
            with patch.object(lab, 'db_cli', return_value=SimpleNamespace(stdout=status)), patch.object(lab, 'sql') as sql:
                with self.assertRaises(RuntimeError):
                    lab.restore_replication()
                sql.assert_not_called()

    def test_preserves_zone_settings_when_restoring_three_replicas(self):
        buf = io.StringIO()
        writer = csv.writer(buf)
        writer.writerow(['target', 'raw_config_sql'])
        writer.writerow(['RANGE default', 'ALTER RANGE default CONFIGURE ZONE USING gc.ttlseconds = 14400, num_replicas = 1'])
        writer.writerow(['TABLE system.public.lease', 'ALTER TABLE system.public.lease CONFIGURE ZONE USING gc.ttlseconds = 600, num_replicas = 1'])
        before = buf.getvalue().encode()
        with tempfile.TemporaryDirectory() as tmp, patch.object(lab, 'LAB', Path(tmp)), patch.object(
                lab, 'db_cli', return_value=SimpleNamespace(stdout=b'id,is_live\n1,true\n2,true\n3,true\n')), patch.object(
                lab, 'sql', return_value=SimpleNamespace(stdout=before)) as sql:
            lab.restore_replication()
            changed = sql.call_args_list[1].args[0]
            self.assertIn('gc.ttlseconds = 14400, num_replicas = 3', changed)
            self.assertIn('gc.ttlseconds = 600, num_replicas = 3', changed)
            self.assertNotIn('num_replicas = 1', changed)
            self.assertEqual((Path(tmp) / 'replication-before.csv').read_bytes(), before)


if __name__ == '__main__':
    unittest.main()
