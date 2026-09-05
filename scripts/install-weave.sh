#!/usr/bin/env bash
# Install or resume a local Weave Workbench stack from one repository checkout.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"

for command_name in docker openssl curl df; do
	command -v "${command_name}" >/dev/null 2>&1 || {
		echo "Weave Workbench requires ${command_name}." >&2
		exit 1
	}
done
docker info >/dev/null
docker compose version >/dev/null

ENV_FILE="${ROOT_DIR}/.env"
VERSION="$(tr -d '[:space:]' < "${ROOT_DIR}/VERSION")"

random_hex() {
	openssl rand -hex "$1"
}

env_value() {
	sed -n "s/^$1=//p" "${ENV_FILE}" | tail -n 1
}

set_env_value() {
	local key="$1" value="$2" temporary
	temporary="$(mktemp "${ENV_FILE}.XXXXXX")"
	awk -v key="${key}" -v value="${value}" '
		BEGIN { replaced = 0 }
		$0 ~ "^" key "=" { if (!replaced) print key "=" value; replaced = 1; next }
		{ print }
		END { if (!replaced) print key "=" value }
	' "${ENV_FILE}" > "${temporary}"
	chmod 600 "${temporary}"
	mv "${temporary}" "${ENV_FILE}"
}

if [[ ! -f "${ENV_FILE}" ]]; then
	umask 077
	cat > "${ENV_FILE}" <<EOF
WEAVE_VERSION=${VERSION}
NPM_REGISTRY=${NPM_REGISTRY:-https://registry.npmjs.org}
DOCKER_BUILD_HTTP_PROXY=
DOCKER_BUILD_HTTPS_PROXY=
DOCKER_BUILD_ALL_PROXY=
DOCKER_BUILD_NO_PROXY=localhost,127.0.0.1
CONTAINER_HTTP_PROXY=
CONTAINER_HTTPS_PROXY=
CONTAINER_ALL_PROXY=
CONTAINER_NO_PROXY=localhost,127.0.0.1,db,weave,workbench
WEAVE_API_BIND_ADDRESS=127.0.0.1
WEAVE_API_PORT=${WEAVE_API_PORT:-8080}
JWT_SECRET=$(random_hex 32)
WEAVE_ADMIN_USER=admin
WEAVE_ADMIN_PASS=$(random_hex 18)
POSTGRES_PASSWORD=$(random_hex 24)
WEAVE_SECRET_KEY=$(random_hex 32)
WEAVE_SECRET_KEY_FILE=
DEEPSEEK_API_KEY=
ANTHROPIC_API_KEY=
OPENAI_API_KEY=
OPENAI_BASE_URL=
OPENAI_MODELS=gpt-5.2,gpt-5.3-codex,gpt-5.4
WEAVE_API_KEY=
WEAVE_RUNTIME_SERVER_URL=
WEAVE_RUNTIME_TOKEN=
WEAVE_PLATFORM_IMAGE=weave-platform
WEAVE_WORKBENCH_IMAGE=weave-workbench
WORKBENCH_PUBLIC_AUTHORITY=127.0.0.1:${WORKBENCH_PORT:-3080}
WORKBENCH_BIND_ADDRESS=127.0.0.1
WORKBENCH_PORT=${WORKBENCH_PORT:-3080}
WORKBENCH_DATA_PATH=./data/workbench
WORKBENCH_WORKSPACE_PATH=./data/workspaces
EOF
	echo "Created private configuration at ${ENV_FILE}"
fi

for required_key in JWT_SECRET WEAVE_ADMIN_PASS POSTGRES_PASSWORD WEAVE_SECRET_KEY; do
	[[ -n "$(env_value "${required_key}")" ]] || {
		echo "${required_key} is empty in ${ENV_FILE}; set it before installing." >&2
		exit 1
	}
done

PROJECT_NAME="${COMPOSE_PROJECT_NAME:-$(basename "${ROOT_DIR}" | tr '[:upper:]' '[:lower:]' | sed -E 's/[^a-z0-9_-]+/-/g; s/^[-_]+//; s/[-_]+$//')}"
PROJECT_NAME="${PROJECT_NAME:-weave}"
COMPOSE=(docker compose --env-file "${ENV_FILE}" -f docker-compose.platform.yml --project-name "${PROJECT_NAME}")
BUILD_COMMIT="$(git rev-parse HEAD 2>/dev/null || printf unknown)"
INSTALL_BUILD_ARGS=(--build)
remote_platform="ghcr.io/jinyitao123/weave-platform:${VERSION}"
remote_workbench="ghcr.io/jinyitao123/weave-workbench:${VERSION}"
if [[ "$(env_value WEAVE_PLATFORM_IMAGE)" == "weave-platform" \
	&& "$(env_value WEAVE_WORKBENCH_IMAGE)" == "weave-workbench" ]] \
	&& docker manifest inspect "${remote_platform}" >/dev/null 2>&1 \
	&& docker manifest inspect "${remote_workbench}" >/dev/null 2>&1; then
	set_env_value WEAVE_PLATFORM_IMAGE "${remote_platform}"
	set_env_value WEAVE_WORKBENCH_IMAGE "${remote_workbench}"
	INSTALL_BUILD_ARGS=(--no-build)
	"${COMPOSE[@]}" pull db weave workbench workbench-gateway
fi

require_numeric_port() {
	local name="$1" value="$2"
	if [[ ! "${value}" =~ ^[0-9]+$ ]] || ((value < 1 || value > 65535)); then
		echo "${name} must be a port from 1 to 65535; got ${value}." >&2
		exit 1
	fi
}

require_available_port() {
	local port="$1" label="$2" container_id owner own_container=0
	while read -r container_id owner; do
		[[ -n "${container_id}" ]] || continue
		if [[ "${owner:-}" != "${PROJECT_NAME}" ]]; then
			echo "Port ${port} for ${label} is already used by Docker container ${container_id}." >&2
			echo "Set a different ${label} port before rerunning the installer." >&2
			exit 1
		fi
		own_container=1
	done < <(docker ps --filter "publish=${port}" --format '{{.ID}} {{.Label "com.docker.compose.project"}}')
	if ((own_container == 0)) && (exec 3<>"/dev/tcp/127.0.0.1/${port}") 2>/dev/null; then
		exec 3>&-
		exec 3<&-
		echo "Port ${port} for ${label} is already used by a host process." >&2
		echo "Set a different ${label} value in .env, then rerun the installer." >&2
		exit 1
	fi
}

WEAVE_API_PORT="$(env_value WEAVE_API_PORT)"
WEAVE_API_PORT="${WEAVE_API_PORT:-8080}"
WORKBENCH_PORT="$(env_value WORKBENCH_PORT)"
WORKBENCH_PORT="${WORKBENCH_PORT:-3080}"
require_numeric_port WEAVE_API_PORT "${WEAVE_API_PORT}"
require_numeric_port WORKBENCH_PORT "${WORKBENCH_PORT}"
require_available_port "${WEAVE_API_PORT}" WEAVE_API_PORT
require_available_port "${WORKBENCH_PORT}" WORKBENCH_PORT

if [[ "${INSTALL_BUILD_ARGS[0]}" == "--build" && "${WEAVE_SKIP_DISK_CHECK:-0}" != "1" ]]; then
	available_kb="$(df -Pk "${ROOT_DIR}" | awk 'NR == 2 { print $4 }')"
	required_kb=$((8 * 1024 * 1024))
	if [[ "${available_kb}" =~ ^[0-9]+$ ]] && ((available_kb < required_kb)); then
		available_gb=$((available_kb / 1024 / 1024))
		echo "A local image build needs about 8 GB free; ${available_gb} GB is available." >&2
		echo "Free disk space or set WEAVE_SKIP_DISK_CHECK=1 if Docker stores images on another disk." >&2
		exit 1
	fi
fi

echo "Starting Weave services..."
BUILD_COMMIT="${BUILD_COMMIT}" "${COMPOSE[@]}" up -d "${INSTALL_BUILD_ARGS[@]}" db weave

for _ in {1..60}; do
	if curl --silent --fail --max-time 2 "http://127.0.0.1:${WEAVE_API_PORT}/v1/ready" >/dev/null 2>&1; then
		break
	fi
	sleep 2
done
curl --silent --fail --max-time 2 "http://127.0.0.1:${WEAVE_API_PORT}/v1/ready" >/dev/null

WEAVE_API_KEY="$(env_value WEAVE_API_KEY)"
if [[ -z "${WEAVE_API_KEY}" ]]; then
	bootstrap_output="$("${COMPOSE[@]}" exec -T weave /usr/local/bin/weave bootstrap --api-url http://weave:8080)"
	WEAVE_API_KEY="$(sed -n 's/^[[:space:]]*"api_key": "\([^"]*\)",\{0,1\}$/\1/p' <<<"${bootstrap_output}")"
	if [[ -z "${WEAVE_API_KEY}" ]]; then
		echo "Workbench API key already exists but is not present in .env." >&2
		echo "Restore the owner API key from your secret store, set WEAVE_API_KEY, and rerun." >&2
		exit 1
	fi
	set_env_value WEAVE_API_KEY "${WEAVE_API_KEY}"
fi

WEAVE_RUNTIME_TOKEN="$(env_value WEAVE_RUNTIME_TOKEN)"
if [[ -z "${WEAVE_RUNTIME_TOKEN}" ]]; then
	runtime_list="$(curl --silent --fail --header "Authorization: Bearer ${WEAVE_API_KEY}" "http://127.0.0.1:${WEAVE_API_PORT}/v1/runtimes")"
	if ! grep -Eq '"runtimes"[[:space:]]*:[[:space:]]*\[[[:space:]]*\]' <<<"${runtime_list}"; then
		echo "A runtime already exists but its token is not present in .env." >&2
		echo "Register a new local runtime in Workbench settings, set WEAVE_RUNTIME_TOKEN, and rerun." >&2
		exit 1
	fi
	runtime_output="$(curl --silent --fail --request POST \
		--header "Authorization: Bearer ${WEAVE_API_KEY}" \
		--header 'Content-Type: application/json' \
		--data '{"name":"Local Workbench Runtime"}' \
		"http://127.0.0.1:${WEAVE_API_PORT}/v1/runtimes")"
	WEAVE_RUNTIME_TOKEN="$(sed -n 's/.*"token":"\([^"]*\)".*/\1/p' <<<"${runtime_output}")"
	[[ -n "${WEAVE_RUNTIME_TOKEN}" ]] || {
		echo "The local runtime was created but its token could not be read." >&2
		exit 1
	}
	set_env_value WEAVE_RUNTIME_TOKEN "${WEAVE_RUNTIME_TOKEN}"
fi

echo "Starting Workbench and the local runtime..."
BUILD_COMMIT="${BUILD_COMMIT}" "${COMPOSE[@]}" --profile runtime up -d "${INSTALL_BUILD_ARGS[@]}" runtime workbench workbench-gateway

for _ in {1..60}; do
	workbench_state="$("${COMPOSE[@]}" ps --format json workbench 2>/dev/null || true)"
	if [[ "${workbench_state}" == *'"Health":"healthy"'* ]]; then
		break
	fi
	sleep 2
done

startup_token="$("${COMPOSE[@]}" logs --no-color workbench 2>/dev/null | sed -n 's/.*?token=\([^[:space:]]*\).*/\1/p' | tail -n 1)"

workbench_state="$("${COMPOSE[@]}" ps --format json workbench 2>/dev/null || true)"
if [[ "${workbench_state}" != *'"Health":"healthy"'* ]]; then
	echo "Workbench did not become healthy. Inspect it with:" >&2
	echo "  docker compose --env-file .env -f docker-compose.platform.yml logs workbench workbench-gateway" >&2
	exit 1
fi

public_status="$(curl --silent --output /dev/null --write-out '%{http_code}' --max-time 5 "http://127.0.0.1:${WORKBENCH_PORT}/" || true)"
if [[ "${public_status}" != "200" && "${public_status}" != "401" ]]; then
	echo "Workbench gateway returned HTTP ${public_status:-unreachable}." >&2
	exit 1
fi

runtime_online=""
for _ in {1..30}; do
	runtime_list="$(curl --silent --fail --header "Authorization: Bearer ${WEAVE_API_KEY}" "http://127.0.0.1:${WEAVE_API_PORT}/v1/runtimes" || true)"
	if grep -Eq '"online"[[:space:]]*:[[:space:]]*true' <<<"${runtime_list}"; then
		runtime_online=1
		break
	fi
	sleep 2
done
if [[ -z "${runtime_online}" ]]; then
	echo "The local runtime did not connect to Weave. Inspect it with:" >&2
	echo "  docker compose --env-file .env -f docker-compose.platform.yml --profile runtime logs runtime" >&2
	exit 1
fi

echo
echo "Weave Workbench is ready."
if [[ -n "${startup_token}" ]]; then
	echo "Open: http://127.0.0.1:${WORKBENCH_PORT}/?token=${startup_token}"
else
	echo "Open: http://127.0.0.1:${WORKBENCH_PORT}/"
fi
echo "Next: add a model provider in Workbench, then create or import your first team."
echo "Configuration and data remain under ${ROOT_DIR}."
