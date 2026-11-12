#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${repo_root}"

case "${1:-}" in
  test)
    exec go test ./...
    ;;
  vet)
    exec go vet ./...
    ;;
  fmt-check)
    unformatted=$(git ls-files --cached --others --exclude-standard -z -- '*.go' | xargs -0 -r gofmt -l)
    if [[ -n "${unformatted}" ]]; then
      printf 'Run gofmt on:\n%s\n' "${unformatted}" >&2
      exit 1
    fi
    ;;
  lint)
    source "${repo_root}/tools/versions.sh"
    linter="${repo_root}/bin/golangci-lint"
    if [[ ! -x "${linter}" ]] ||
      [[ "$("${linter}" version --short)" != "${GOLANGCI_LINT_VERSION#v}" ]]; then
      "${repo_root}/tools/install-golangci-lint.sh"
    fi
    exec "${linter}" run ./...
    ;;
  verify-go)
    "${repo_root}/task.sh" fmt-check
    "${repo_root}/task.sh" vet
    "${repo_root}/task.sh" lint
    "${repo_root}/task.sh" test
    ;;
  *)
    printf 'Usage: %s {test|vet|fmt-check|lint|verify-go}\n' "$0" >&2
    exit 2
    ;;
esac
