#!/bin/sh
set -eu

ROOT_DIR="$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)"
TEMPLATE_DIR="${ROOT_DIR}/deploy/huggingface"
OUTPUT_DIR="${ROOT_DIR}/.dist/hf-space"
PANEL_HTML_SOURCE="${HF_MANAGEMENT_HTML_SOURCE:-${ROOT_DIR}/../Cli-Proxy-API-Management-Center/dist/management.html}"
PANEL_HTML_FALLBACK="${ROOT_DIR}/../Cli-Proxy-API-Management-Center/dist/index.html"

cd "${ROOT_DIR}"

mkdir -p "${OUTPUT_DIR}"
rm -f "${OUTPUT_DIR}/README.md" "${OUTPUT_DIR}/Dockerfile" "${OUTPUT_DIR}/entrypoint.sh" "${OUTPUT_DIR}/config.template.yaml" "${OUTPUT_DIR}/config.example.yaml" "${OUTPUT_DIR}/management.html" "${OUTPUT_DIR}/server"

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -trimpath \
  -ldflags="-s -w" \
  -o "${OUTPUT_DIR}/server" \
  ./cmd/server

cp "${TEMPLATE_DIR}/README.md" "${OUTPUT_DIR}/README.md"
cp "${TEMPLATE_DIR}/Dockerfile" "${OUTPUT_DIR}/Dockerfile"
cp "${TEMPLATE_DIR}/entrypoint.sh" "${OUTPUT_DIR}/entrypoint.sh"
cp "${TEMPLATE_DIR}/config.template.yaml" "${OUTPUT_DIR}/config.template.yaml"
cp "${ROOT_DIR}/config.example.yaml" "${OUTPUT_DIR}/config.example.yaml"

if [ -f "${PANEL_HTML_SOURCE}" ]; then
  cp "${PANEL_HTML_SOURCE}" "${OUTPUT_DIR}/management.html"
elif [ -f "${PANEL_HTML_FALLBACK}" ]; then
  cp "${PANEL_HTML_FALLBACK}" "${OUTPUT_DIR}/management.html"
fi

chmod +x "${OUTPUT_DIR}/server" "${OUTPUT_DIR}/entrypoint.sh"

printf '%s\n' "HF bundle ready at ${OUTPUT_DIR}"
