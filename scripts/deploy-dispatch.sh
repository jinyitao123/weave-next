#!/usr/bin/env bash
set -euo pipefail
umask 077

state_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
original_command="${SSH_ORIGINAL_COMMAND:-}"
if [[ ! "$original_command" =~ ^deploy[[:space:]]+([0-9a-f]{40})$ ]]; then
  echo 'Only deploy <full lowercase commit SHA> is permitted.' >&2
  exit 2
fi

deploy_sha="${BASH_REMATCH[1]}"
deploy_main="$state_dir/deploy-main.sh"
if [[ ! -x "$deploy_main" ]]; then
  echo 'The standard deployment runner is not installed.' >&2
  exit 2
fi
exec "$deploy_main" "$deploy_sha"
