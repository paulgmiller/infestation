#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "${repo_root}/tools/versions.sh"

mkdir -p "${repo_root}/bin"
curl -sSfL https://golangci-lint.run/install.sh |
  sh -s -- -b "${repo_root}/bin" "${GOLANGCI_LINT_VERSION}"
