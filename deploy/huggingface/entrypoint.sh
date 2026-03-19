#!/bin/sh
set -eu

APP_DIR="/app"
BINARY="${APP_DIR}/server"
TEMPLATE="${APP_DIR}/config.template.yaml"

export HOST="${HOST:-0.0.0.0}"
export PORT="${PORT:-7860}"

if [ -z "${WRITABLE_PATH:-}" ]; then
  if [ -d /data ]; then
    WRITABLE_PATH="/data/app"
  else
    WRITABLE_PATH="/tmp/app"
  fi
fi

CONFIG_PATH="${CONFIG_PATH:-${WRITABLE_PATH}/config.yaml}"
AUTH_DIR="${WRITABLE_PATH}/auths"
LOG_DIR="${WRITABLE_PATH}/logs"

mkdir -p "${WRITABLE_PATH}" "${AUTH_DIR}" "${LOG_DIR}"

if [ ! -f "${CONFIG_PATH}" ]; then
  sed "s|__AUTH_DIR__|${AUTH_DIR}|g" "${TEMPLATE}" > "${CONFIG_PATH}"
fi

exec "${BINARY}" -config "${CONFIG_PATH}"
