#!/usr/bin/env bash
set -euo pipefail
umask 077

expected_current_sha="${1:-}"
[[ "$expected_current_sha" =~ ^[0-9a-f]{64}$ ]] || {
  echo 'Expected the current remote deploy-main.sh SHA-256.' >&2
  exit 2
}

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
target="${WEAVE_DEPLOY_BOOTSTRAP_TARGET:-inoforge}"
remote_script="/home/ubuntu/.local/share/weave-deploy/deploy-main.sh"
remote_next="$remote_script.next"
ssh_opts=(-o BatchMode=yes -o StrictHostKeyChecking=yes)
current_hash="$(ssh "${ssh_opts[@]}" "$target" "sha256sum '$remote_script' | awk '{print \$1}'")"
if [[ "$current_hash" != "$expected_current_sha" ]]; then
  echo 'Remote deploy runner changed since it was reviewed; refusing to overwrite it.' >&2
  exit 3
fi

local_hash="$(shasum -a 256 "$script_dir/deploy-main.sh" | awk '{print $1}')"
scp "${ssh_opts[@]}" "$script_dir/deploy-main.sh" "$target:$remote_next"
ssh "${ssh_opts[@]}" "$target" "set -eu
current=\$(sha256sum '$remote_script' | awk '{print \$1}')
test \"\$current\" = '$expected_current_sha'
next=\$(sha256sum '$remote_next' | awk '{print \$1}')
test \"\$next\" = '$local_hash'
chmod 700 '$remote_next'
mv '$remote_next' '$remote_script'
"
echo "Updated the gated deployment runner on $target ($expected_current_sha -> $local_hash)."
