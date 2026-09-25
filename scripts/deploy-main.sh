#!/usr/bin/env bash
set -euo pipefail
umask 077

expected_sha="${1:-}"
[[ "$expected_sha" =~ ^[0-9a-f]{40}$ ]] || { echo 'Expected a full commit SHA.' >&2; exit 2; }
state_dir="${WEAVE_DEPLOY_STATE_DIR:-$HOME/.local/share/weave-deploy}"
env_file="${WEAVE_DEPLOY_ENV_FILE:-$HOME/.config/weave-main/server.env}"
mkdir -p "$state_dir/releases" "$state_dir/backups"
exec 9>"$state_dir/deploy.lock"
flock -w 1800 9
test -f "$env_file"
IFS= read -r github_token
test -n "$github_token"
IFS= read -r source_archive_sha
[[ "$source_archive_sha" =~ ^[0-9a-f]{64}$ ]] || { echo 'Expected a source archive SHA-256.' >&2; exit 2; }

source_archive="$(mktemp "$state_dir/.source-archive.XXXXXX")"
source_stage=''
cleanup_deploy_files() {
  rm -f -- "$source_archive"
  if [[ -n "$source_stage" && -d "$source_stage" ]]; then rm -rf -- "$source_stage"; fi
}
trap cleanup_deploy_files EXIT
cat > "$source_archive"
archive_size="$(stat -c '%s' "$source_archive")"
[[ "$archive_size" -gt 0 && "$archive_size" -le 157286400 ]] || { echo 'Source archive is empty or exceeds 150 MiB.' >&2; exit 2; }
actual_archive_sha="$(sha256sum "$source_archive" | awk '{print $1}')"
[[ "$actual_archive_sha" == "$source_archive_sha" ]] || { echo 'Source archive SHA-256 does not match the CI-provided digest.' >&2; exit 2; }
tar -tzf "$source_archive" >/dev/null

current_main_sha() {
  DEPLOY_GITHUB_TOKEN="$github_token" python3 - <<'PY'
import json
import os
import re
import urllib.request

token = os.environ.pop('DEPLOY_GITHUB_TOKEN')
request = urllib.request.Request(
    'https://api.github.com/repos/jinyitao123/weave-next/branches/main',
    headers={
        'Authorization': 'Bearer ' + token,
        'Accept': 'application/vnd.github+json',
        'X-GitHub-Api-Version': '2022-11-28',
    },
)
with urllib.request.urlopen(request, timeout=20) as response:
    sha = json.load(response)['commit']['sha']
if not re.fullmatch(r'[0-9a-f]{40}', sha):
    raise SystemExit('GitHub returned an invalid main SHA.')
print(sha)
PY
}

phase=source
trap 'echo "Deployment failed during $phase for $expected_sha; inspect the Actions log and $state_dir/backups. Database backups are never restored automatically." >&2' ERR
upstream_sha="$(current_main_sha)"
if [[ "$upstream_sha" != "$expected_sha" ]]; then
  echo "Deployment superseded: main is now $upstream_sha."
  exit 0
fi

release_dir="$state_dir/releases/$expected_sha"
source_manifest="$state_dir/releases/$expected_sha.source-sha256"
if [[ -d "$release_dir" ]]; then
  if [[ -f "$source_manifest" ]]; then
    [[ "$(tr -d '[:space:]' < "$source_manifest")" == "$source_archive_sha" ]] || {
      echo 'Existing release source archive does not match this CI payload.' >&2
      exit 2
    }
  elif git -C "$release_dir" rev-parse --git-dir >/dev/null 2>&1 \
    && [[ "$(git -C "$release_dir" rev-parse HEAD)" == "$expected_sha" ]] \
    && [[ -z "$(git -C "$release_dir" status --porcelain --untracked-files=normal)" ]]; then
    # Migrate an exact, clean release worktree created by the previous deployment path.
    printf '%s\n' "$source_archive_sha" > "$source_manifest"
  else
    echo 'Existing release directory has no verifiable source manifest; refusing to reuse it.' >&2
    exit 2
  fi
else
  if [[ -f "$source_manifest" ]]; then
    [[ "$(tr -d '[:space:]' < "$source_manifest")" == "$source_archive_sha" ]] || {
      echo 'Staged release manifest does not match this CI payload.' >&2
      exit 2
    }
  fi
  source_stage="$(mktemp -d "$state_dir/releases/.${expected_sha}.stage.XXXXXX")"
  tar -xzf "$source_archive" --no-same-owner --no-same-permissions -C "$source_stage"
  test -s "$source_stage/VERSION"
  test -f "$source_stage/docker-compose.platform.yml"
  test -f "$source_stage/scripts/deployment-state.py"
  printf '%s\n' "$source_archive_sha" > "$source_manifest.next"
  mv "$source_manifest.next" "$source_manifest"
  mv "$source_stage" "$release_dir"
  source_stage=''
fi

phase=runner-update
cp "$release_dir/scripts/deploy-main.sh" "$state_dir/deploy-main.sh.next"
chmod 700 "$state_dir/deploy-main.sh.next"
mv "$state_dir/deploy-main.sh.next" "$state_dir/deploy-main.sh"

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

# Recheck through GitHub's lightweight API before cutover; source bytes were already
# transferred from the CI-verified checkout and verified against its SHA-256.
phase=source-recheck
upstream_sha="$(current_main_sha)"
unset github_token
if [[ "$upstream_sha" != "$expected_sha" ]]; then
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
echo "Deployed main $expected_sha."
