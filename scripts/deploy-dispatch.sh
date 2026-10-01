#!/usr/bin/env bash
set -euo pipefail
umask 077

state_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
original_command="${SSH_ORIGINAL_COMMAND:-}"
if [[ ! "$original_command" =~ ^deploy[[:space:]]+([0-9a-f]{40})[[:space:]]+([0-9a-f]{64})$ ]]; then
  echo 'Only deploy <full lowercase commit SHA> <source runner SHA-256> is permitted.' >&2
  exit 2
fi

deploy_sha="${BASH_REMATCH[1]}"
expected_runner_sha="${BASH_REMATCH[2]}"
IFS= read -r github_token
test -n "$github_token"
IFS= read -r source_archive_sha
[[ "$source_archive_sha" =~ ^[0-9a-f]{64}$ ]] || { echo 'Expected a source archive SHA-256.' >&2; exit 2; }

incoming="$(mktemp -d "$state_dir/.source-runner.XXXXXX")"
trap 'rm -rf -- "$incoming"' EXIT
source_archive="$incoming/source.tar.gz"
source_runner="$incoming/deploy-main.sh"
cat > "$source_archive"
archive_size="$(stat -c '%s' "$source_archive")"
[[ "$archive_size" -gt 0 && "$archive_size" -le 157286400 ]] || { echo 'Source archive is empty or exceeds 150 MiB.' >&2; exit 2; }
actual_archive_sha="$(sha256sum "$source_archive" | awk '{print $1}')"
[[ "$actual_archive_sha" == "$source_archive_sha" ]] || { echo 'Source archive SHA-256 does not match the CI-provided digest.' >&2; exit 2; }
# Extract only a bounded, unique regular runner. No archive path, link or mode
# controls where the executable is written or which installed file is invoked.
python3 - "$source_archive" "$source_runner" <<'PY'
from pathlib import Path
import sys
import tarfile

try:
    with tarfile.open(sys.argv[1], 'r:gz') as archive:
        members = archive.getmembers()
        required = ('VERSION', 'docker-compose.platform.yml', 'scripts/deployment-state.py', 'scripts/deploy-main.sh')
        for name in required:
            matches = [member for member in members if member.name == name]
            if len(matches) != 1 or not matches[0].isfile() or matches[0].size <= 0:
                raise ValueError('incomplete deployment source')
        runners = [member for member in members if member.name == 'scripts/deploy-main.sh']
        if len(runners) != 1 or not runners[0].isfile() or not 0 < runners[0].size <= 128 * 1024:
            raise ValueError('invalid source runner')
        Path(sys.argv[2]).write_bytes(archive.extractfile(runners[0]).read())
except (OSError, tarfile.TarError, ValueError):
    raise SystemExit('Source archive lacks complete regular deployment source or a unique bounded runner.')
PY
actual_runner_sha="$(sha256sum "$source_runner" | awk '{print $1}')"
[[ "$actual_runner_sha" == "$expected_runner_sha" ]] || { echo 'Source runner SHA-256 does not match the CI-verified runner.' >&2; exit 2; }
# Check the selected source before executing any deployment logic from it.
# The source runner retains its own pre-build and pre-cutover main checks.
upstream_sha="$(DEPLOY_GITHUB_TOKEN="$github_token" python3 - <<'PY'
import json
import os
import re
import urllib.request

request = urllib.request.Request(
    'https://api.github.com/repos/jinyitao123/weave-next/branches/main',
    headers={'Authorization': 'Bearer ' + os.environ.pop('DEPLOY_GITHUB_TOKEN'),
             'Accept': 'application/vnd.github+json', 'X-GitHub-Api-Version': '2022-11-28'},
)
with urllib.request.urlopen(request, timeout=20) as response:
    sha = json.load(response)['commit']['sha']
if not re.fullmatch(r'[0-9a-f]{40}', sha):
    raise SystemExit('GitHub returned an invalid main SHA.')
print(sha)
PY
)"
if [[ "$upstream_sha" != "$deploy_sha" ]]; then
  echo "Deployment superseded before source runner execution: main is now $upstream_sha."
  exit 0
fi

echo "Executing verified source runner $deploy_sha ($actual_runner_sha); archive $actual_archive_sha."
# Never invoke state_dir/deploy-main.sh: it may predate this release's behavior.
# The outer dispatcher owns temporary cleanup; the child executes exactly the
# authenticated archive bytes and receives the unchanged deployment framing.
{
  printf '%s\n%s\n' "$github_token" "$source_archive_sha"
  cat "$source_archive"
} | (export WEAVE_DEPLOY_STATE_DIR="$state_dir"; exec bash "$source_runner" "$deploy_sha")
