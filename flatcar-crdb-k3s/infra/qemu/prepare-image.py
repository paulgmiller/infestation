#!/usr/bin/env python3
"""Download and verify a pinned Flatcar qcow2 base image; never boot a VM."""
import argparse
import bz2
import hashlib
from pathlib import Path
import re
import shutil
import subprocess
import tempfile

BASE = 'flatcar_production_qemu_image.img'


def extract_image(archive, digests, destination):
    expected = next((line.split()[0] for line in digests.read_text().splitlines()
                     if re.fullmatch(r'[0-9a-fA-F]{128}\s+\*?' + re.escape(archive.name), line)), None)
    with archive.open('rb') as source:
        actual = hashlib.file_digest(source, 'sha512').hexdigest()
    if expected is None or actual.lower() != expected.lower():
        raise ValueError('Flatcar archive SHA512 does not match its vendor DIGESTS file')
    with tempfile.NamedTemporaryFile(dir=destination.parent, delete=False) as out:
        temporary = Path(out.name)
        try:
            with bz2.open(archive, 'rb') as source:
                shutil.copyfileobj(source, out)
            out.flush()
            with temporary.open('rb') as source:
                if source.read(4) != b'QFI\xfb':
                    raise ValueError('Downloaded image is not qcow2')
            if destination.exists():
                with destination.open('rb') as source, temporary.open('rb') as candidate:
                    if hashlib.file_digest(source, 'sha512').digest() != hashlib.file_digest(candidate, 'sha512').digest():
                        raise FileExistsError('Existing base image differs; refusing to replace it')
            else:
                temporary.chmod(0o444)
                temporary.replace(destination)
        finally:
            temporary.unlink(missing_ok=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--version', required=True)
    parser.add_argument('--state-dir', type=Path, default=Path('state'))
    args = parser.parse_args()
    if not re.fullmatch(r'[0-9]+\.[0-9]+\.[0-9]+', args.version):
        parser.error('--version must pin an exact numeric Flatcar release')
    args.state_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    cache = args.state_dir / ('image-' + args.version)
    cache.mkdir(mode=0o700, exist_ok=True)
    archive, digests = cache / (BASE + '.bz2'), cache / (BASE + '.bz2.DIGESTS')
    url = f'https://stable.release.flatcar-linux.net/amd64-usr/{args.version}/'
    for path in (archive, digests):
        if not path.exists():
            partial = path.with_name(path.name + '.part')
            subprocess.run(['curl', '-fL', '--retry', '3', '-o', str(partial), url + path.name], check=True)
            partial.replace(path)
    destination = args.state_dir / f'flatcar-{args.version}.qcow2'
    extract_image(archive, digests, destination)
    print(f'Verified base image: {destination.resolve()}')


if __name__ == '__main__':
    main()
