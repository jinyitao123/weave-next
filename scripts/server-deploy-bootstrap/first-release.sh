#!/usr/bin/env bash
set -euo pipefail
umask 077

operation="${1:-}"
expected_sha="${2:-}"
ghcr_user="${3:-}"
[[ "$operation" == "deploy" ]] || { echo "Rollback is unavailable before the first Server release." >&2; exit 2; }
[[ "$expected_sha" =~ ^[0-9a-f]{40}$ ]] || { echo "Expected a full commit SHA." >&2; exit 2; }
[[ "$ghcr_user" =~ ^[A-Za-z0-9-]+$ ]] || { echo "Invalid GHCR user." >&2; exit 2; }

state_dir="${WEAVE_DEPLOY_STATE_DIR:-$HOME/.local/share/weave-server-deploy}"
source_dir="${WEAVE_DEPLOY_SOURCE_DIR:-$HOME/weave-server-source}"
release_dir="$state_dir/releases/$expected_sha"
IFS= read -r github_token
IFS= read -r ghcr_token
test -n "$github_token"
test -n "$ghcr_token"

fetch_main() (
  export GIT_CONFIG_COUNT=1 GIT_TERMINAL_PROMPT=0
  export GIT_CONFIG_KEY_0=http.https://github.com/.extraheader
  export GIT_CONFIG_VALUE_0="AUTHORIZATION: basic $(printf '%s' "x-access-token:$github_token" | base64 | tr -d '\n')"
  timeout 600 git -c http.version=HTTP/1.1 -C "$source_dir" fetch --no-tags --depth 1 origin main
)

fetch_main
upstream_sha="$(git -C "$source_dir" rev-parse origin/main)"
[[ "$upstream_sha" == "$expected_sha" ]] || { echo "Server main is now $upstream_sha; bootstrap deployment is superseded." >&2; exit 1; }
if [[ ! -d "$release_dir" ]]; then
  git -C "$source_dir" worktree add --detach "$release_dir" "$expected_sha"
fi
test -x "$release_dir/scripts/deploy-release.sh"
printf '%s\n%s\n' "$github_token" "$ghcr_token" | \
  "$release_dir/scripts/deploy-release.sh" deploy "$expected_sha" "$ghcr_user"
