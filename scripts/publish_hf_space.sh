#!/usr/bin/env zsh
set -euo pipefail
emulate -L zsh

ROOT_DIR="${0:A:h:h}"
PACKAGE_SCRIPT="${ROOT_DIR}/scripts/package_hf_space.sh"
OUTPUT_DIR="${ROOT_DIR}/.dist/hf-space"
HF_PYTHON="${HF_PYTHON:-${HOME}/.local/opt/hf-cli/bin/python3}"
TOKEN_FILE="${HF_TOKEN_FILE:-${HOME}/.cache/huggingface/token}"
HF_API_ENDPOINT="${HF_API_ENDPOINT:-https://huggingface.co}"

SPACE_ID="${1:-${HF_SPACE_ID:-}}"
CLIENT_API_KEY_VALUE="${HF_CLIENT_API_KEY:-${CLIENT_API_KEY:-}}"
MANAGEMENT_PASSWORD_VALUE="${HF_MANAGEMENT_PASSWORD:-${MANAGEMENT_PASSWORD:-}}"
PGSTORE_DSN_VALUE="${HF_PGSTORE_DSN:-${PGSTORE_DSN:-}}"
PGSTORE_SCHEMA_VALUE="${HF_PGSTORE_SCHEMA:-${PGSTORE_SCHEMA:-}}"
PGSTORE_LOCAL_PATH_VALUE="${HF_PGSTORE_LOCAL_PATH:-${PGSTORE_LOCAL_PATH:-}}"
TOKEN_ATLAS_API_KEY_VALUE="${HF_TOKEN_ATLAS_API_KEY:-${TOKEN_ATLAS_API_KEY:-}}"
COMMIT_MESSAGE="${HF_COMMIT_MESSAGE:-deploy hf bundle}"
WAIT_ENABLED="${HF_WAIT:-1}"
WAIT_MAX="${HF_WAIT_MAX:-50}"
WAIT_INTERVAL="${HF_WAIT_INTERVAL:-2}"
RECREATE="${HF_RECREATE:-0}"

usage() {
  cat <<'EOF'
Usage:
  ./scripts/publish_hf_space.sh <owner/space>

Optional environment variables:
  HF_SPACE_ID                 Space id if not passed as argv
  HF_CLIENT_API_KEY           Set/update the CLIENT_API_KEY secret
  HF_MANAGEMENT_PASSWORD      Set/update the MANAGEMENT_PASSWORD secret
  HF_PGSTORE_DSN              Set/update the PGSTORE_DSN secret
  HF_PGSTORE_SCHEMA           Set/update the PGSTORE_SCHEMA secret
  HF_PGSTORE_LOCAL_PATH       Set/update the PGSTORE_LOCAL_PATH secret
  HF_TOKEN_ATLAS_API_KEY      Set/update the TOKEN_ATLAS_API_KEY secret
  HF_COMMIT_MESSAGE           Upload commit message
  HF_RECREATE=1               Delete and recreate the Space before upload
  HF_WAIT=0                   Skip runtime polling after upload
  HF_WAIT_MAX=50              Max polling attempts
  HF_WAIT_INTERVAL=2          Poll interval in seconds
EOF
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || {
    print -u2 -- "Missing command: $1"
    exit 1
  }
}

enable_proxy() {
  if [[ -f "${HOME}/.zshrc" ]]; then
    source "${HOME}/.zshrc" >/dev/null 2>&1 || true
  fi

  if command -v proxyon >/dev/null 2>&1; then
    proxyon >/dev/null 2>&1 || proxyon || true
  fi
}

set_space_secret() {
  local repo_id="$1"
  local key="$2"
  local value="$3"
  local token

  [[ -n "${value}" ]] || return 0
  [[ -r "${TOKEN_FILE}" ]] || {
    print -u2 -- "HF token file not found at ${TOKEN_FILE}"
    exit 1
  }

  token="$(<"${TOKEN_FILE}")"

  curl -fsS \
    -X POST \
    -H "Authorization: Bearer ${token}" \
    -H "Content-Type: application/json" \
    --data "{\"key\":\"${key}\",\"value\":\"${value}\"}" \
    "${HF_API_ENDPOINT}/api/spaces/${repo_id}/secrets" >/dev/null

  print -- "Set secret: ${key}"
}

poll_runtime() {
  local repo_id="$1"
  local token json stage err host status_code
  local runtime_url="https://huggingface.co/api/spaces/${repo_id}/runtime"

  if [[ ! -r "${TOKEN_FILE}" ]]; then
    print -u2 -- "Token file not found at ${TOKEN_FILE}; skipping runtime check."
    return 0
  fi

  token="$(<"${TOKEN_FILE}")"
  host="${repo_id//\//-}.hf.space"

  for ((i = 1; i <= WAIT_MAX; ++i)); do
    json="$(curl -fsS -H "Authorization: Bearer ${token}" "${runtime_url}")"
    stage="$(print -r -- "${json}" | "${HF_PYTHON}" -c 'import sys,json; d=json.load(sys.stdin); print(d.get("stage",""))')"
    err="$(print -r -- "${json}" | "${HF_PYTHON}" -c 'import sys,json; d=json.load(sys.stdin); print(d.get("errorMessage") or "")')"

    if [[ -n "${err}" ]]; then
      print -- "[${i}/${WAIT_MAX}] stage=${stage} error=${err}"
    else
      print -- "[${i}/${WAIT_MAX}] stage=${stage}"
    fi

    if [[ "${stage}" == "RUNNING" ]]; then
      status_code="$(curl -sS -o /dev/null -w "%{http_code}" -H "Authorization: Bearer ${token}" "https://${host}/healthz" || true)"
      print -- "healthz=${status_code}"
      return 0
    fi

    if [[ "${stage}" == "PAUSED" && -n "${err}" ]]; then
      return 1
    fi

    sleep "${WAIT_INTERVAL}"
  done

  print -u2 -- "Timed out waiting for ${repo_id} to reach RUNNING."
  return 1
}

main() {
  if [[ -z "${SPACE_ID}" ]]; then
    usage
    exit 1
  fi

  require_cmd hf
  require_cmd curl

  if [[ ! -x "${HF_PYTHON}" ]]; then
    print -u2 -- "HF python not found at ${HF_PYTHON}"
    exit 1
  fi

  enable_proxy

  cd "${ROOT_DIR}"
  "${PACKAGE_SCRIPT}"

  if [[ "${RECREATE}" == "1" ]]; then
    printf 'y\n' | hf repos delete "${SPACE_ID}" --type space --missing-ok
  fi

  hf repos create "${SPACE_ID}" --type space --space-sdk docker --private --exist-ok

  set_space_secret "${SPACE_ID}" "CLIENT_API_KEY" "${CLIENT_API_KEY_VALUE}"
  set_space_secret "${SPACE_ID}" "MANAGEMENT_PASSWORD" "${MANAGEMENT_PASSWORD_VALUE}"
  set_space_secret "${SPACE_ID}" "PGSTORE_DSN" "${PGSTORE_DSN_VALUE}"
  set_space_secret "${SPACE_ID}" "PGSTORE_SCHEMA" "${PGSTORE_SCHEMA_VALUE}"
  set_space_secret "${SPACE_ID}" "PGSTORE_LOCAL_PATH" "${PGSTORE_LOCAL_PATH_VALUE}"
  set_space_secret "${SPACE_ID}" "TOKEN_ATLAS_API_KEY" "${TOKEN_ATLAS_API_KEY_VALUE}"

  hf upload "${SPACE_ID}" "${OUTPUT_DIR}" . --repo-type space --commit-message "${COMMIT_MESSAGE}"

  if [[ "${WAIT_ENABLED}" == "0" ]]; then
    print -- "Upload complete."
    return 0
  fi

  poll_runtime "${SPACE_ID}"
}

main "$@"
