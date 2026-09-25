#!/usr/bin/env bash
set -euo pipefail
umask 077

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
target="${WEAVE_DEPLOY_BOOTSTRAP_TARGET:-inoforge}"
state_dir="/home/ubuntu/.local/share/weave-deploy"
ssh_opts=(-o BatchMode=yes -o StrictHostKeyChecking=yes)

ssh "${ssh_opts[@]}" "$target" 'set -eu
state=/home/ubuntu/.local/share/weave-deploy
test ! -e "$state/dispatch.sh"
test ! -e "$state/deploy-main.sh"
install -d -m 700 "$state"
'
scp "${ssh_opts[@]}" "$script_dir/deploy-dispatch.sh" "$target:$state_dir/dispatch.sh.next"
scp "${ssh_opts[@]}" "$script_dir/deploy-main.sh" "$target:$state_dir/deploy-main.sh.next"
ssh "${ssh_opts[@]}" "$target" 'set -eu
state=/home/ubuntu/.local/share/weave-deploy
test ! -e "$state/dispatch.sh"
test ! -e "$state/deploy-main.sh"
chmod 700 "$state/dispatch.sh.next" "$state/deploy-main.sh.next"
mv "$state/dispatch.sh.next" "$state/dispatch.sh"
mv "$state/deploy-main.sh.next" "$state/deploy-main.sh"
test -x "$state/dispatch.sh"
test -x "$state/deploy-main.sh"
'
echo "Installed the fixed-SHA dispatcher and current deployment runner on $target."
