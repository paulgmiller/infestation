#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
IMAGE="$(python3 -c 'import json; print(json.load(open("cluster.json"))["cockroach_image"])')"
FIRST_IP="$(python3 -c 'import json; print(json.load(open("cluster.json"))["nodes"][0]["ip"])')"
if [[ ! -f pki/admin/client.root.key ]]; then
  echo 'Missing pki/admin/client.root.key; run python3 generate.py pki first.' >&2
  exit 1
fi
# Run on a Linux workstation able to reach node 1 on port 26257.
CRDB=(docker run --rm --network host --user "$(id -u):$(id -g)" \
  -v "$PWD/pki/admin:/certs:ro" --entrypoint=/cockroach/cockroach "$IMAGE")
echo "Initializing brand-new CockroachDB cluster at $FIRST_IP:26257 (ONLY run once)..."
"${CRDB[@]}" init --certs-dir=/certs --host="$FIRST_IP:26257"
echo "Creating database and certificate-authenticated kine user..."
"${CRDB[@]}" sql --certs-dir=/certs --host="$FIRST_IP:26257" --execute="
CREATE DATABASE IF NOT EXISTS kubernetes;
CREATE USER IF NOT EXISTS kine;
GRANT ALL ON DATABASE kubernetes TO kine;
GRANT ALL ON SCHEMA kubernetes.public TO kine;
"
echo 'Bootstrap complete. K3s services should now be able to access the datastore.'
