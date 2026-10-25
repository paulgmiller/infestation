import bz2
import hashlib
import importlib.util
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('prepare_image', Path(__file__).with_name('prepare-image.py'))
image = importlib.util.module_from_spec(spec)
spec.loader.exec_module(image)


class ImageTests(unittest.TestCase):
    def test_verifies_archive_and_preserves_existing_image(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            archive, digests, target = root / 'image.bz2', root / 'DIGESTS', root / 'base.qcow2'
            content = b'QFI\xfb' + bytes(100)
            archive.write_bytes(bz2.compress(content))
            digest = hashlib.sha512(archive.read_bytes()).hexdigest()
            digests.write_text(f'# SHA512 HASH\n{digest}  {archive.name}\n')
            image.extract_image(archive, digests, target)
            self.assertEqual(target.read_bytes(), content)
            self.assertEqual(target.stat().st_mode & 0o777, 0o444)
            image.extract_image(archive, digests, target)
            target.chmod(0o600)
            target.write_bytes(b'EXISTING DISK')
            with self.assertRaises(FileExistsError):
                image.extract_image(archive, digests, target)
            self.assertEqual(target.read_bytes(), b'EXISTING DISK')
            archive.write_bytes(b'corrupt download')
            with self.assertRaises(ValueError):
                image.extract_image(archive, digests, target)
            self.assertEqual(target.read_bytes(), b'EXISTING DISK')


if __name__ == '__main__':
    unittest.main()
