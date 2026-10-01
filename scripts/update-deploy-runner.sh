#!/usr/bin/env bash
set -euo pipefail
umask 077

expected_current_sha="${1:-}"
expected_dispatch_sha="${2:-}"
source_commit="${3:-}"
[[ "$expected_current_sha" =~ ^[0-9a-f]{64}$ && "$expected_dispatch_sha" =~ ^[0-9a-f]{64}$ && "$source_commit" =~ ^[0-9a-f]{40}$ ]] || {
  echo 'Expected current runner SHA-256, current dispatcher SHA-256, and reviewed full source commit SHA.' >&2
  exit 2
}

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
target="${WEAVE_DEPLOY_BOOTSTRAP_TARGET:-inoforge}"
remote_state="/home/ubuntu/.local/share/weave-deploy"
remote_script="$remote_state/deploy-main.sh"
remote_dispatch="$remote_state/dispatch.sh"
ssh_opts=(-o BatchMode=yes -o StrictHostKeyChecking=yes)
# Install an immutable reviewed source version, never uncommitted local files.
stage="$(mktemp -d)"
trap 'rm -rf -- "$stage"' EXIT
git -C "$script_dir" show "$source_commit:scripts/deploy-main.sh" > "$stage/deploy-main.sh"
git -C "$script_dir" show "$source_commit:scripts/deploy-dispatch.sh" > "$stage/dispatch.sh"
local_hash="$(shasum -a 256 "$stage/deploy-main.sh" | awk '{print $1}')"
local_dispatch_hash="$(shasum -a 256 "$stage/dispatch.sh" | awk '{print $1}')"
# Check before transferring and again under the deployment lock before cutover.
ssh "${ssh_opts[@]}" "$target" "set -eu
runner=\$(sha256sum '$remote_script' | awk '{print \$1}')
dispatch=\$(sha256sum '$remote_dispatch' | awk '{print \$1}')
test \"\$runner\" = '$expected_current_sha'
test \"\$dispatch\" = '$expected_dispatch_sha'
"
scp "${ssh_opts[@]}" "$stage/deploy-main.sh" "$target:$remote_script.next"
scp "${ssh_opts[@]}" "$stage/dispatch.sh" "$target:$remote_dispatch.next"
ssh "${ssh_opts[@]}" "$target" "set -eu
exec 9>'$remote_state/deploy.lock'
flock -w 1800 9
runner=\$(sha256sum '$remote_script' | awk '{print \$1}')
dispatch=\$(sha256sum '$remote_dispatch' | awk '{print \$1}')
test \"\$runner\" = '$expected_current_sha'
test \"\$dispatch\" = '$expected_dispatch_sha'
next=\$(sha256sum '$remote_script.next' | awk '{print \$1}')
next_dispatch=\$(sha256sum '$remote_dispatch.next' | awk '{print \$1}')
test \"\$next\" = '$local_hash'
test \"\$next_dispatch\" = '$local_dispatch_hash'
chmod 700 '$remote_script.next' '$remote_dispatch.next'
mv '$remote_script.next' '$remote_script'
mv '$remote_dispatch.next' '$remote_dispatch'
"
echo "Installed reviewed source entry $source_commit on $target (runner $local_hash; dispatcher $local_dispatch_hash)."
