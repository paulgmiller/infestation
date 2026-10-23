#!/usr/bin/env python3
"""Disposable single-node Flatcar lab. All state and secrets live in ./lab/."""
import argparse
import bz2
import hashlib
import json
import os
from pathlib import Path
import socket
import subprocess
import sys

HERE = Path(__file__).resolve().parent
LAB = HERE / 'lab'
FLATCAR = '4757.2.2'
BASE = 'flatcar_production_qemu_image.img'


def run(*args, **kwargs):
    return subprocess.run([str(a) for a in args], check=True, **kwargs)


def ssh_args():
    return ['ssh', '-i', str(LAB / 'id_ed25519'), '-p', '2222',
            '-o', 'IdentitiesOnly=yes', '-o', 'StrictHostKeyChecking=accept-new',
            '-o', f'UserKnownHostsFile={LAB / "known_hosts"}',
            '-o', 'ConnectTimeout=5', 'core@127.0.0.1']


def prepare():
    LAB.mkdir(mode=0o700, exist_ok=True)
    LAB.chmod(0o700)
    if not (LAB / 'id_ed25519').exists():
        run('ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', LAB / 'id_ed25519')
    config = json.loads((HERE / 'cluster.json').read_text())
    config.update(single_node_lab=True,
                  ssh_authorized_key=(LAB / 'id_ed25519.pub').read_text().strip(),
                  nodes=[{'name': 'kube1', 'ip': '10.0.2.15'}])
    (LAB / 'cluster.json').write_text(json.dumps(config, indent=2) + '\n')
    if not (LAB / 'pki').exists():
        run(sys.executable, HERE / 'generate.py', 'pki', '--config', LAB / 'cluster.json', '--state-dir', LAB)
    run(sys.executable, HERE / 'generate.py', 'ignition', '--config', LAB / 'cluster.json', '--state-dir', LAB)
    archive = LAB / (BASE + '.bz2')
    digests = LAB / (BASE + '.bz2.DIGESTS')
    url = f'https://stable.release.flatcar-linux.net/amd64-usr/{FLATCAR}/'
    for path in (archive, digests):
        if not path.exists():
            run('curl', '-fL', '--retry', '3', '-o', str(path) + '.part', url + path.name)
            Path(str(path) + '.part').rename(path)
    with archive.open('rb') as src:
        digest = hashlib.file_digest(src, 'sha512').hexdigest()
    if f'{digest}  {archive.name}' not in digests.read_text():
        raise RuntimeError('Flatcar image SHA512 mismatch')
    image = LAB / BASE
    if not image.exists():
        import shutil
        with bz2.open(archive, 'rb') as src, Path(str(image) + '.part').open('wb') as dst:
            shutil.copyfileobj(src, dst)
        Path(str(image) + '.part').rename(image)
    print('Prepared lab; Flatcar SHA512 verified.')


def start():
    if (LAB / 'qemu.pid').exists():
        raise RuntimeError('qemu.pid exists; inspect the running VM before starting another')
    disk = LAB / 'node.qcow2'
    if not disk.exists():
        run('qemu-img', 'create', '-f', 'qcow2', '-F', 'qcow2', '-b', LAB / BASE, disk, '40G')
    run('qemu-system-x86_64', '-name', 'infestation-kube1', '-enable-kvm', '-cpu', 'host',
        '-smp', '4', '-m', '6144', '-drive', f'if=virtio,format=qcow2,file={disk}',
        '-fw_cfg', f'name=opt/org.flatcar-linux/config,file={LAB / "output/kube1.ign"}',
        '-netdev', 'user,id=eth0,hostfwd=tcp:127.0.0.1:2222-:22,hostfwd=tcp:127.0.0.1:6443-:6443,hostfwd=tcp:127.0.0.1:26257-:26257',
        '-device', 'virtio-net-pci,netdev=eth0', '-display', 'none',
        '-serial', f'file:{LAB / "console.log"}',
        '-monitor', f'unix:{LAB / "monitor.sock"},server=on,wait=off',
        '-pidfile', LAB / 'qemu.pid', '-daemonize')


def sql(statement):
    config = json.loads((LAB / 'cluster.json').read_text())
    run('docker', 'run', '--rm', '--network', 'host', '--user',
        f'{os.getuid()}:{os.getgid()}',
        '-v', f'{LAB / "pki/admin"}:/certs:ro', '--entrypoint=/cockroach/cockroach',
        config['cockroach_image'], 'sql', '--certs-dir=/certs', '--host=127.0.0.1:26257',
        '--execute', statement)


def kubeconfig():
    result = run(*ssh_args(), 'sudo cat /etc/rancher/k3s/k3s.yaml', capture_output=True)
    path = LAB / 'kubeconfig'
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, 'wb') as dst:
        dst.write(result.stdout)
    path.chmod(0o600)
    print(f'Kubeconfig: {path}')


def cilium(args):
    import tarfile
    version = 'v0.20.1'
    archive = LAB / 'cilium-linux-amd64.tar.gz'
    sums = LAB / 'cilium-linux-amd64.tar.gz.sha256sum'
    for path in (archive, sums):
        if not path.exists():
            run('curl', '-fL', '--retry', '3', '-o', str(path) + '.part',
                f'https://github.com/cilium/cilium-cli/releases/download/{version}/{path.name}')
            Path(str(path) + '.part').rename(path)
    with archive.open('rb') as src:
        digest = hashlib.file_digest(src, 'sha256').hexdigest()
    if digest != sums.read_text().split()[0]:
        raise RuntimeError('Cilium CLI SHA256 mismatch')
    with tarfile.open(archive) as src:
        src.extract('cilium', LAB, filter='data')
    run(LAB / 'cilium', '--kubeconfig', LAB / 'kubeconfig', *args)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=['prepare', 'start', 'ssh', 'sql', 'bootstrap-db', 'kubeconfig', 'cilium', 'smoke', 'stop'])
    parser.add_argument('args', nargs=argparse.REMAINDER)
    args = parser.parse_args()
    if args.command == 'prepare':
        prepare()
    elif args.command == 'start':
        start()
    elif args.command == 'ssh':
        run(*ssh_args(), *args.args)
    elif args.command == 'sql':
        sql(' '.join(args.args))
    elif args.command == 'bootstrap-db':
        # start-single-node initializes itself. Never run cockroach init here.
        sql((HERE / 'bootstrap.sql').read_text())
    elif args.command == 'kubeconfig':
        kubeconfig()
    elif args.command == 'cilium':
        cilium(args.args)
    elif args.command == 'smoke':
        run(*ssh_args(), 'sudo k3s kubectl apply -f -', input=(HERE / 'smoke.yaml').read_bytes())
        run(*ssh_args(), 'sudo k3s kubectl wait --for=condition=Ready node/kube1 --timeout=45s')
        run(*ssh_args(), 'sudo k3s kubectl rollout status -n lab-smoke deployment/web --timeout=180s')
        run(*ssh_args(), 'sudo k3s kubectl wait -n lab-smoke --for=condition=complete job/dns-http --timeout=180s')
        run(*ssh_args(), 'sudo k3s kubectl logs -n lab-smoke -l job-name=dns-http --all-containers --prefix')
    elif args.command == 'stop':
        # Ask this VM to shut down gracefully; never signal an unchecked PID.
        with socket.socket(socket.AF_UNIX) as conn:
            conn.connect(str(LAB / 'monitor.sock'))
            conn.sendall(b'system_powerdown\n')


if __name__ == '__main__':
    main()
