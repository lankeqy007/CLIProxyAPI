#!/bin/sh
set -eu

APP_DIR="/CLIProxyAPI"
BINARY="${APP_DIR}/CLIProxyAPI"
HF_TEMPLATE="${APP_DIR}/config.hf.template.yaml"

is_hugging_face_space() {
  [ -n "${SPACE_ID:-}" ] || [ -n "${SPACE_HOST:-}" ]
}

if is_hugging_face_space; then
  export HOST="${HOST:-0.0.0.0}"
  export PORT="${PORT:-8317}"

  if [ -z "${WRITABLE_PATH:-}" ] && [ -z "${writable_path:-}" ]; then
    if [ -d /data ]; then
      export WRITABLE_PATH="/data/cliproxyapi"
    else
      export WRITABLE_PATH="/tmp/cliproxyapi"
    fi
  fi

  RUNTIME_ROOT="${WRITABLE_PATH:-${writable_path:-}}"
  CONFIG_PATH="${CONFIG_PATH:-${RUNTIME_ROOT}/config.yaml}"
  AUTH_DIR="${RUNTIME_ROOT}/auths"
  LOG_DIR="${RUNTIME_ROOT}/logs"

  mkdir -p "${RUNTIME_ROOT}" "${AUTH_DIR}" "${LOG_DIR}"

  if [ ! -f "${CONFIG_PATH}" ]; then
    sed "s|__AUTH_DIR__|${AUTH_DIR}|g" "${HF_TEMPLATE}" > "${CONFIG_PATH}"
    echo "Initialized Hugging Face config at ${CONFIG_PATH}"
  fi

  if [ -z "${MANAGEMENT_PASSWORD:-}" ]; then
    echo "Warning: MANAGEMENT_PASSWORD is not set; remote management UI will stay disabled." >&2
  fi

  exec "${BINARY}" -config "${CONFIG_PATH}" "$@"
fi

exec "${BINARY}" "$@"
