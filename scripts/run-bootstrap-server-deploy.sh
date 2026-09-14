#!/usr/bin/env bash
set -euo pipefail

public_key_file="${WEAVE_SERVER_DEPLOY_PUBLIC_KEY_FILE:-/Users/jinyitao/.config/weave-server-deploy/id_ed25519.pub}"
repository="${WEAVE_BOOTSTRAP_REPOSITORY:-jinyitao123/weave-next}"
workflow="bootstrap-server-deploy.yml"
mode="${1:-check}"
case "$mode" in
  check|--dispatch) ;;
  *) echo "Usage: run-bootstrap-server-deploy.sh [--dispatch]" >&2; exit 2 ;;
esac

test -f "$public_key_file"
public_key="$(tr -d '\r\n' < "$public_key_file")"
[[ "$public_key" =~ ^ssh-ed25519\ [A-Za-z0-9+/]+={0,3}(\ [A-Za-z0-9._@+-]+)?$ ]] || {
  echo "Invalid ed25519 public key file: $public_key_file" >&2
  exit 2
}
command -v gh >/dev/null 2>&1 || { echo "gh is required." >&2; exit 2; }
gh auth status >/dev/null

if [[ "$mode" == "--dispatch" ]]; then
  gh workflow run "$workflow" --repo "$repository" --ref main \
    -f confirmation=bootstrap-weave-server \
    -f server_public_key="$public_key"
  echo "Bootstrap workflow dispatched for $repository."
else
  echo "Bootstrap input is ready from $public_key_file. Re-run with --dispatch after the workflow is on main."
fi
unset public_key
