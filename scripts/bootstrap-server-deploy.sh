#!/usr/bin/env bash
set -euo pipefail
umask 077

state_dir="${WEAVE_DEPLOY_STATE_DIR:-$HOME/.local/share/weave-server-deploy}"
source_dir="${WEAVE_DEPLOY_SOURCE_DIR:-$HOME/weave-server-source}"
env_file="${WEAVE_DEPLOY_ENV_FILE:-$HOME/.config/weave-server/server.env}"
config_dir="$(dirname "$env_file")"
repository_url="${WEAVE_SERVER_REPOSITORY_URL:-https://github.com/jinyitao123/weave-server.git}"
payload_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/server-deploy-bootstrap" && pwd)"

IFS= read -r public_key
[[ "$public_key" =~ ^ssh-ed25519\ [A-Za-z0-9+/]+={0,3}(\ [A-Za-z0-9._@+-]+)?$ ]] || {
  echo "A single OpenSSH ed25519 public key is required." >&2
  exit 2
}
key_material="$(awk '{print $2}' <<<"$public_key")"
IFS= read -r server_env_b64
test -n "$server_env_b64"
temporary_env="$(mktemp)"
trap 'rm -f "$temporary_env"' EXIT
printf '%s' "$server_env_b64" | base64 -d > "$temporary_env"
python3 - "$temporary_env" <<'PY_VALIDATE'
import re
import sys
from pathlib import Path

path = Path(sys.argv[1])
text = path.read_text()
required = {
    "POSTGRES_PASSWORD", "JWT_SECRET", "WEAVE_SECRET_KEY", "WEAVE_ADMIN_PASS",
    "WORKBENCH_PUBLIC_AUTHORITY", "WORKBENCH_DATA_PATH", "WORKBENCH_WORKSPACE_PATH",
    "WORKBENCH_BIND_ADDRESS", "OPENAI_BASE_URL", "OPENAI_API_KEY", "OPENAI_MODELS",
    "DEFAULT_MODEL",
}
seen = set()
values = {}
for line in text.splitlines():
    if not line or line.startswith("#"):
        continue
    match = re.fullmatch(r"([A-Z][A-Z0-9_]*)=(.*)", line)
    if not match or match.group(1) in seen:
        raise SystemExit("Invalid or duplicate server.env entry")
    seen.add(match.group(1))
    values[match.group(1)] = match.group(2)
    if match.group(1) in required and not match.group(2):
        raise SystemExit("Required server.env value is empty")
missing = sorted(required - seen)
if missing:
    raise SystemExit("Missing required server.env entries: " + ", ".join(missing))
for key in ("WORKBENCH_DATA_PATH", "WORKBENCH_WORKSPACE_PATH"):
    target = Path(values[key])
    if not target.is_absolute():
        raise SystemExit(key + " must be an absolute path")
    target.mkdir(parents=True, exist_ok=True, mode=0o700)
PY_VALIDATE

mkdir -p "$state_dir/releases" "$state_dir/backups" "$state_dir/logs" "$config_dir" "$HOME/.ssh"
chmod 700 "$state_dir" "$config_dir" "$HOME/.ssh"
if [[ ! -d "$source_dir/.git" ]]; then
  if [[ -d "$source_dir" && -n "$(find "$source_dir" -mindepth 1 -maxdepth 1 -print -quit)" ]]; then
    echo "Existing Server source directory is not an empty Git repository." >&2
    exit 1
  fi
  mkdir -p "$source_dir"
  git -C "$source_dir" init --initial-branch=main
  git -C "$source_dir" remote add origin "$repository_url"
else
  existing_origin="$(git -C "$source_dir" remote get-url origin)"
  [[ "$existing_origin" == "$repository_url" ]] || { echo "Existing Server source remote does not match." >&2; exit 1; }
fi

if [[ -e "$env_file" ]]; then
  cmp -s "$temporary_env" "$env_file" || { echo "Existing Server environment differs from bootstrap input." >&2; exit 1; }
else
  install -m 600 "$temporary_env" "$env_file"
fi
chmod 600 "$env_file"
cat > "$config_dir/required-settings.txt" <<'SETTINGS'
POSTGRES_PASSWORD
JWT_SECRET
WEAVE_SECRET_KEY
WEAVE_ADMIN_PASS
WORKBENCH_PUBLIC_AUTHORITY
WORKBENCH_DATA_PATH
WORKBENCH_WORKSPACE_PATH
WORKBENCH_BIND_ADDRESS
OPENAI_BASE_URL
OPENAI_API_KEY
OPENAI_MODELS
DEFAULT_MODEL
SETTINGS
chmod 600 "$config_dir/required-settings.txt"

install -m 700 "$payload_dir/first-release.sh" "$state_dir/first-release.sh"
install -m 700 "$payload_dir/restricted-deploy-command.sh" "$state_dir/restricted-deploy-command.sh"

authorized_keys="$HOME/.ssh/authorized_keys"
touch "$authorized_keys"
chmod 600 "$authorized_keys"
forced_command="$state_dir/restricted-deploy-command.sh"
entry="restrict,command=\"$forced_command\" ssh-ed25519 $key_material weave-server-deploy"
if awk -v key="$key_material" '{ for (i=1; i<=NF; i++) if ($i == key) found=1 } END { exit !found }' "$authorized_keys"; then
  grep -Fqx "$entry" "$authorized_keys" || {
    echo "The Server deployment public key already exists with different restrictions." >&2
    exit 1
  }
else
  temporary="$authorized_keys.next"
  cat "$authorized_keys" > "$temporary"
  printf '%s\n' "$entry" >> "$temporary"
  chmod 600 "$temporary"
  mv "$temporary" "$authorized_keys"
fi

echo "Weave Server deployment directories, environment and restricted command are installed."
