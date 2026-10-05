#!/usr/bin/env bash
# This is the standard entry point for refreshing the Weave platform.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"

BUILD_COMMIT="$(git rev-parse HEAD)"
if [[ -n "$(git status --porcelain)" ]]; then
	BUILD_COMMIT="${BUILD_COMMIT}-dirty"
fi

# Docker Compose auto-loads .env only from the current project directory.
# Feature worktrees intentionally do not copy the repository's secret-bearing
# .env, so refreshing from a worktree used to recreate the shared service with
# empty provider/encryption keys. Resolve the common Git directory and reuse
# the main checkout's .env without copying or printing it.
compose_env_file=""
COMMON_GIT_DIR="$(git rev-parse --path-format=absolute --git-common-dir)"
SHARED_ROOT_DIR="$(dirname "${COMMON_GIT_DIR}")"
if [[ ! -f "${ROOT_DIR}/.env" ]]; then
	SHARED_ENV_FILE="${SHARED_ROOT_DIR}/.env"
	if [[ -f "${SHARED_ENV_FILE}" ]]; then
		compose_env_file="${SHARED_ENV_FILE}"
	fi
fi

# Compose otherwise derives the project name from the current worktree
# directory (for example team-forge-t05) and attempts to create a second stack
# on the shared ports. All worktrees refresh the one platform stack owned by
# the main checkout.
COMPOSE_PROJECT_NAME="$(basename "${SHARED_ROOT_DIR}")"

echo "Refreshing Weave at build commit ${BUILD_COMMIT}"
if [[ -n "${compose_env_file}" ]]; then
	BUILD_COMMIT="${BUILD_COMMIT}" docker compose -f docker-compose.platform.yml --env-file "${compose_env_file}" \
		--project-name "${COMPOSE_PROJECT_NAME}" up -d --build
else
	BUILD_COMMIT="${BUILD_COMMIT}" docker compose -f docker-compose.platform.yml \
		--project-name "${COMPOSE_PROJECT_NAME}" up -d --build
fi

health_output=""
for _ in {1..60}; do
	if health_output="$(curl --silent --show-error --connect-timeout 1 --max-time 2 http://localhost:8080/v1/health 2>/dev/null)" &&
		[[ "$(jq -r '.build_commit // empty' <<<"${health_output}")" == "${BUILD_COMMIT}" ]]; then
		echo "Weave refresh complete: ${health_output}"
		exit 0
	fi
	sleep 2
done

echo "timed out waiting for /v1/health build_commit to equal ${BUILD_COMMIT}" >&2
if [[ -n "${health_output}" ]]; then
	echo "last health response: ${health_output}" >&2
fi
exit 1
