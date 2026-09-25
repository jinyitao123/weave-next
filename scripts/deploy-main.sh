#!/usr/bin/env bash
set -euo pipefail
umask 077

expected_sha="${1:-}"
[[ "$expected_sha" =~ ^[0-9a-f]{40}$ ]] || { echo 'Expected a full commit SHA.' >&2; exit 2; }
state_dir="${WEAVE_DEPLOY_STATE_DIR:-$HOME/.local/share/weave-deploy}"
source_dir="${WEAVE_DEPLOY_SOURCE_DIR:-$HOME/weave-main-source}"
env_file="${WEAVE_DEPLOY_ENV_FILE:-$HOME/.config/weave-main/server.env}"
mkdir -p "$state_dir/releases" "$state_dir/backups"
exec 9>"$state_dir/deploy.lock"
flock -w 1800 9
test -f "$env_file"
IFS= read -r github_token
test -n "$github_token"

phase=source-initialization
if ! git -C "$source_dir" rev-parse --git-dir >/dev/null 2>&1; then
  if [[ -e "$source_dir" ]] && [[ -n "$(find "$source_dir" -mindepth 1 -maxdepth 1 -print -quit)" ]]; then
    echo 'Deployment source path exists but is not a Git repository; refusing to reuse or overwrite it.' >&2
    exit 2
  fi
  mkdir -p "$source_dir"
  git -C "$source_dir" init --quiet
  git -C "$source_dir" remote add origin https://github.com/jinyitao123/weave-next.git
fi
origin_url="$(git -C "$source_dir" remote get-url origin 2>/dev/null || true)"
case "$origin_url" in
  https://github.com/jinyitao123/weave-next.git) ;;
  *) echo 'Deployment source origin does not match jinyitao123/weave-next; refusing to fetch.' >&2; exit 2 ;;
esac

fetch_main() (
  # The job token is repository-scoped, short-lived, and never stored in Git or passed to builds.
  export GIT_CONFIG_COUNT=1 GIT_TERMINAL_PROMPT=0
  export GIT_CONFIG_KEY_0=http.https://github.com/.extraheader
  export GIT_CONFIG_VALUE_0="AUTHORIZATION: basic $(printf '%s' "x-access-token:$github_token" | base64 | tr -d '\n')"
  timeout 600 git -c http.version=HTTP/1.1 -C "$source_dir" fetch --no-tags --depth 1 origin main
)

phase=fetch
trap 'echo "Deployment failed during $phase for $expected_sha; inspect the Actions log and $state_dir/backups. Database backups are never restored automatically." >&2' ERR
fetch_main
upstream_sha="$(git -C "$source_dir" rev-parse origin/main)"
if [[ "$upstream_sha" != "$expected_sha" ]]; then
  echo "Deployment superseded: main is now $upstream_sha."
  exit 0
fi
release_dir="$state_dir/releases/$expected_sha"
if [[ ! -d "$release_dir" ]]; then
  git -C "$source_dir" worktree add --detach "$release_dir" "$expected_sha"
fi
test "$(git -C "$release_dir" rev-parse HEAD)" = "$expected_sha"
test -z "$(git -C "$release_dir" status --porcelain --untracked-files=normal)"
version="$(tr -d '[:space:]' < "$release_dir/VERSION")"
release_env="$state_dir/releases/$expected_sha.env"
printf '%s\n' \
  "BUILD_COMMIT=$expected_sha" \
  "WORKBENCH_BUILD_COMMIT=${expected_sha:0:7}" \
  "WEAVE_VERSION=$version" \
  "WEAVE_PLATFORM_IMAGE=weave-main-platform:$expected_sha" \
  "WEAVE_WORKBENCH_IMAGE=weave-main-workbench:$expected_sha" > "$release_env"
compose=(sudo docker compose --project-name weave-main --project-directory "$release_dir"
  --env-file "$env_file" --env-file "$release_env" -f "$release_dir/docker-compose.platform.yml")
"${compose[@]}" config --quiet
phase=build
"${compose[@]}" build weave workbench
# Recheck after a long build so an obsolete release is never selected for cutover.
fetch_main
unset github_token
if [[ "$(git -C "$source_dir" rev-parse origin/main)" != "$expected_sha" ]]; then
  echo 'A newer main commit arrived during the build; leaving running services unchanged.'
  exit 0
fi
phase=backup
backup_dir="$state_dir/backups/$(date -u +%Y%m%dT%H%M%SZ)-$expected_sha"
mkdir -p "$backup_dir"
cp -p "$env_file" "$backup_dir/server.env"
if [[ -n "$("${compose[@]}" ps --status running -q db)" ]]; then
  "${compose[@]}" exec -T db pg_dump -U weave -d weave -Fc > "$backup_dir/database.dump"
fi
phase=prepare-workbench-storage
python3 "$release_dir/scripts/deployment-state.py" prepare-workbench-storage "$env_file" "$state_dir"
phase=api-startup
"${compose[@]}" up -d --no-build --wait --wait-timeout 120 db weave
phase=bootstrap
python3 "$release_dir/scripts/deployment-state.py" bootstrap "$env_file" "$state_dir" -- "${compose[@]}"
phase=workbench-startup
"${compose[@]}" up -d --no-build --wait --wait-timeout 180 workbench workbench-gateway
phase=verification
python3 "$release_dir/scripts/deployment-state.py" verify "$env_file" "$state_dir" "$expected_sha"
ln -sfn "$release_dir" "$state_dir/current"
cp "$release_dir/scripts/deploy-main.sh" "$state_dir/deploy-main.sh.next"
chmod 700 "$state_dir/deploy-main.sh.next"
mv "$state_dir/deploy-main.sh.next" "$state_dir/deploy-main.sh"
echo "Deployed main $expected_sha."
