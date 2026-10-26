#!/usr/bin/env python3
"""Complete a prepared Azure cluster, retaining admin credentials on the workstation."""
import argparse
import csv
from contextlib import contextmanager
import io
import json
import os
from pathlib import Path
import socket
import subprocess
import time

PROJECT = Path(__file__).resolve().parent
SOURCE = PROJECT.parent.parent


def private_json(path, value):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    os.fchmod(fd, 0o600)
    with os.fdopen(fd, 'w') as output:
        json.dump(value, output, indent=2)
        output.write('\n')
    path.chmod(0o600)


def cluster_id(result):
    if result.returncode:
        return None
    rows = list(csv.reader(io.StringIO(result.stdout)))
    if len(rows) != 2 or len(rows[1]) != 1:
        raise RuntimeError('Unexpected cluster identity response; refusing initialization')
    import uuid
    return str(uuid.UUID(rows[1][0]))


def initialization_allowed(marker, allow_new):
    return allow_new and not marker.get('clusterID') and not marker.get('initializationAttempted')


def record_identity(marker, current):
    if marker.get('clusterID') and marker['clusterID'] != current:
        raise RuntimeError('Database identity changed; refusing to bootstrap another cluster')
    return {**marker, 'clusterID': current}


def wait_for(action, description, timeout=600):
    deadline = time.monotonic() + timeout
    while True:
        result = action()
        if result.returncode == 0:
            return result
        if time.monotonic() >= deadline:
            raise RuntimeError(f'Timed out waiting for {description}: {result.stderr[-1500:]}')
        time.sleep(3)


class Bootstrap:
    def __init__(self, state, cluster, outputs, key, cilium):
        self.state, self.cluster, self.outputs = state, cluster, outputs
        self.key, self.cilium = key.resolve(), cilium
        self.servers = [n for n in cluster['nodes'] if n.get('role', 'server') == 'server']
        self.db_nodes = [n for n in cluster['nodes'] if n.get('cockroachdb', True)]
        self.kubeconfig = state / 'kubeconfig'

    def run(self, args, **kwargs):
        command = [str(x) for x in args]
        try:
            return subprocess.run(command, capture_output=True, text=True, timeout=kwargs.pop('timeout', 120), **kwargs)
        except subprocess.TimeoutExpired:
            return subprocess.CompletedProcess(command, 124, '', 'Command timed out')

    def ssh(self, node):
        address = self.outputs.get('publicIPs', {}).get(node['name']) or node['ip']
        return ['ssh', '-i', self.key, '-o', 'IdentitiesOnly=yes', '-o', 'BatchMode=yes',
                '-o', 'StrictHostKeyChecking=accept-new', '-o', f'UserKnownHostsFile={self.state / "known_hosts"}',
                '-o', 'ConnectTimeout=5', '-o', 'ServerAliveInterval=15', '-o', 'ServerAliveCountMax=3',
                'core@' + address]

    @contextmanager
    def tunnel(self, node, remote_host, remote_port):
        with socket.socket() as sock:
            sock.bind(('127.0.0.1', 0))
            port = sock.getsockname()[1]
        args = self.ssh(node)
        args[-1:-1] = ['-N', '-o', 'ExitOnForwardFailure=yes', '-L', f'127.0.0.1:{port}:{remote_host}:{remote_port}']
        with (self.state / 'ssh-tunnel.log').open('a') as log:
            process = subprocess.Popen([str(x) for x in args], stdout=log, stderr=log)
            try:
                for _ in range(100):
                    if process.poll() is not None:
                        raise RuntimeError('SSH tunnel failed; inspect private ssh-tunnel.log')
                    try:
                        with socket.create_connection(('127.0.0.1', port), timeout=.2):
                            break
                    except OSError:
                        time.sleep(.1)
                else:
                    raise RuntimeError('SSH tunnel did not start')
                yield port
            finally:
                process.terminate()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()

    def db(self, port, *args, timeout=120):
        return self.run(['docker', 'run', '--rm', '--network', 'host', '--user', f'{os.getuid()}:{os.getgid()}',
                         '-v', f'{self.state / "pki/admin"}:/certs:ro', '--entrypoint=/cockroach/cockroach',
                         self.cluster['cockroach_image'], *args, '--certs-dir=/certs', f'--host=127.0.0.1:{port}'], timeout=timeout)

    def sql_identity(self, port):
        return self.db(port, 'sql', '--format=csv', '--execute=SELECT crdb_internal.cluster_id()', timeout=15)

    def database(self, allow_new):
        path = self.state / 'bootstrap.json'
        marker = json.loads(path.read_text()) if path.exists() else {}
        for node in self.db_nodes:
            wait_for(lambda: self.run([*self.ssh(node), 'sudo systemctl is-active cockroachdb']),
                     f"database service on {node['name']}")
        # Try each database through a server gateway before considering init.
        gateway = self.servers[0]
        identity = None
        for node in self.db_nodes:
            with self.tunnel(gateway, node['ip'], 26257) as port:
                identity = cluster_id(self.sql_identity(port))
                if identity:
                    marker = record_identity(marker, identity)
                    private_json(path, marker)
                    break
        if not identity:
            with self.tunnel(gateway, self.db_nodes[0]['ip'], 26257) as port:
                if initialization_allowed(marker, allow_new):
                    # Persist intent before the RPC: an interruption cannot cause a second init.
                    marker['initializationAttempted'] = True
                    private_json(path, marker)
                    result = self.db(port, 'init')
                    if result.returncode:
                        print('Initialization returned an error; checking whether it completed before retrying anything.', flush=True)
                elif not marker.get('clusterID'):
                    raise RuntimeError('Database unavailable. New-cluster initialization requires --initialize-new-cluster; an attempted init is never repeated automatically')
                identity = cluster_id(wait_for(lambda: self.sql_identity(port), 'existing database identity'))
                marker = record_identity(marker, identity)
                private_json(path, marker)
        with self.tunnel(gateway, self.db_nodes[0]['ip'], 26257) as port:
            # A single idempotent batch; no credential upload to VMs is required.
            sql = (SOURCE / 'bootstrap.sql').read_text()
            wait_for(lambda: self.db(port, 'sql', '--execute', sql), 'Kine database setup')
            def healthy():
                result = self.db(port, 'node', 'status', '--ranges', '--format=csv')
                if result.returncode:
                    return result
                rows = list(csv.DictReader(io.StringIO(result.stdout)))
                live = [r for r in rows if r.get('is_live') == 'true']
                if len(live) < len(self.db_nodes) or any(int(r['ranges_unavailable']) or int(r['ranges_underreplicated']) for r in live):
                    return subprocess.CompletedProcess([], 1, '', 'Database replicas not healthy yet')
                return result
            status = wait_for(healthy, 'healthy database replicas')
            print(status.stdout, end='')
        print(f'Database identity verified: {identity}', flush=True)

    def kubectl(self, *args):
        return self.run(['kubectl', '--kubeconfig', self.kubeconfig, '--request-timeout=15s', *args])

    def kubernetes(self):
        node = self.servers[0]
        result = wait_for(lambda: self.run([*self.ssh(node), 'sudo cat /etc/rancher/k3s/k3s.yaml']), 'admin kubeconfig')
        raw_path = self.state / 'kubeconfig-source'
        fd = os.open(raw_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, 'w') as output:
            output.write(result.stdout)
        try:
            converted = self.run(['kubectl', '--kubeconfig', raw_path, 'config', 'view', '--raw', '-o', 'json'])
            if converted.returncode:
                raise RuntimeError('Could not decode admin kubeconfig')
            config = json.loads(converted.stdout)
        finally:
            raw_path.unlink(missing_ok=True)
        endpoint = self.outputs.get('apiEndpoint')
        if not endpoint:
            raise RuntimeError('Automated bootstrap requires apiLoadBalancer=true for a durable kubeconfig endpoint')
        config['clusters'][0]['name'] = 'azure'
        config['clusters'][0]['cluster']['server'] = endpoint
        config['users'][0]['name'] = 'azure-admin'
        config['contexts'] = [{'name': 'azure', 'context': {'cluster': 'azure', 'user': 'azure-admin'}}]
        config['current-context'] = 'azure'
        private_json(self.kubeconfig, config)
        wait_for(lambda: self.kubectl('get', '--raw=/readyz'), 'public API TLS and readiness')
        existing = self.kubectl('-n', 'kube-system', 'get', 'daemonset', 'cilium', '--ignore-not-found', '-o', 'name')
        if existing.returncode:
            raise RuntimeError(existing.stderr)
        if not existing.stdout.strip():
            # With kube-proxy enabled, the API Service VIP works before pod networking
            # and avoids pinning Cilium bootstrap to one server's private address.
            args = [self.cilium, '--kubeconfig', self.kubeconfig, 'install', '--version', '1.20.4']
            for setting in ('operator.replicas=2', 'kubeProxyReplacement=false',
                            'ipam.operator.clusterPoolIPv4PodCIDRList=10.42.0.0/16',
                            'cni.binPath=/opt/cni/bin', 'cni.confPath=/etc/cni/net.d',
                            'k8sServiceHost=10.43.0.1', 'k8sServicePort=443'):
                args += ['--set', setting]
            installed = self.run(args)
            if installed.returncode:
                raise RuntimeError(installed.stderr)
        ready = subprocess.run([str(self.cilium), '--kubeconfig', str(self.kubeconfig), 'status', '--wait', '--wait-duration=10m'])
        ready.check_returncode()
        for node in self.cluster['nodes']:
            result = self.kubectl('wait', '--for=condition=Ready', f'node/{node["name"]}', '--timeout=90s')
            if result.returncode:
                raise RuntimeError(result.stderr)
        replacement = self.kubectl('-n', 'kube-system', 'get', 'configmap', 'cilium-config', '-o', 'jsonpath={.data.kube-proxy-replacement}')
        if replacement.returncode or replacement.stdout != 'false':
            raise RuntimeError('Unexpected kube-proxy replacement setting')
        marker = json.loads((self.state / 'bootstrap.json').read_text())
        private_json(self.state / 'bootstrap.json', {**marker, 'kubernetesReady': True})
        print(f'Bootstrap complete. Kubeconfig: {self.kubeconfig}', flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--state-dir', type=Path, required=True)
    parser.add_argument('--cluster', type=Path, required=True)
    parser.add_argument('--outputs', type=Path, required=True)
    parser.add_argument('--ssh-private-key', type=Path, required=True)
    parser.add_argument('--cilium', default='cilium')
    parser.add_argument('--initialize-new-cluster', action='store_true')
    args = parser.parse_args()
    worker = Bootstrap(args.state_dir.resolve(), json.loads(args.cluster.read_text()),
                       json.loads(args.outputs.read_text()), args.ssh_private_key, args.cilium)
    worker.database(args.initialize_new_cluster)
    worker.kubernetes()


if __name__ == '__main__':
    main()
