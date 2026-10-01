#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
python3 "${ROOT_DIR}/tools/depguard/check_depguard.py" --root "${ROOT_DIR}" "$@"

(cd "${ROOT_DIR}" && go run ./tools/productsql -root "${ROOT_DIR}")
(cd "${ROOT_DIR}" && go run ./tools/loomimports -root "${ROOT_DIR}")
