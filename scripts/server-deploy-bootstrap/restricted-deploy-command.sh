#!/usr/bin/env bash
set -euo pipefail

original_command="${SSH_ORIGINAL_COMMAND:-}"
read -r operation release_sha ghcr_user extra <<<"$original_command"
case "$operation" in
  deploy|rollback) ;;
  *) echo "Unsupported deployment operation." >&2; exit 2 ;;
esac
[[ "$release_sha" =~ ^[0-9a-f]{40}$ ]] || { echo "Expected a full commit SHA." >&2; exit 2; }
[[ "$ghcr_user" =~ ^[A-Za-z0-9-]+$ ]] || { echo "Invalid GHCR user." >&2; exit 2; }
[[ -z "${extra:-}" ]] || { echo "Unexpected deployment arguments." >&2; exit 2; }

state_dir="${WEAVE_DEPLOY_STATE_DIR:-$HOME/.local/share/weave-server-deploy}"
if [[ -x "$state_dir/deploy-release.sh" ]]; then
  exec "$state_dir/deploy-release.sh" "$operation" "$release_sha" "$ghcr_user"
fi
exec "$state_dir/first-release.sh" "$operation" "$release_sha" "$ghcr_user"
