#!/usr/bin/env bash
set -euo pipefail

# Exercises the managed OCI runtime path against a real Docker daemon without
# building a production image. A static Linux fake-model is mounted into a
# digest-pinned Alpine container, then llama-swap loads it through the normal
# inference router and the Python OpenAI SDK.

readonly repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly fixture_dir="${repo_root}/test/integration/managed-runtime-container"
# The fixture's YAML and Stage request intentionally use this exact public
# image, so do not expose an override that would only change the preload step.
readonly image="docker.io/library/alpine:3.20"
readonly api_key="managed-runtime-container-test-key"
readonly runtime_version="container-it-v1"
readonly temp_dir="$(mktemp -d /tmp/llama-swap-managed-container.XXXXXX)"
readonly build_dir="${repo_root}/build/managed-runtime-container-it-$$"
server_pid=""
container_name=""

cleanup() {
  if [[ -n "${server_pid}" ]]; then
    kill "${server_pid}" >/dev/null 2>&1 || true
    wait "${server_pid}" >/dev/null 2>&1 || true
  fi
  if [[ -n "${container_name}" ]]; then
    docker rm --force "${container_name}" >/dev/null 2>&1 || true
  fi
  rm -rf "${temp_dir}"
  rm -rf "${build_dir}"
}
trap cleanup EXIT INT TERM

for command in docker curl go python3; do
  command -v "${command}" >/dev/null || {
    echo "required command is unavailable: ${command}" >&2
    exit 1
  }
done
python3 -c 'import openai' >/dev/null || {
  echo "Python package openai is required for this integration test" >&2
  exit 1
}

case "$(docker version --format '{{.Server.Arch}}')" in
  arm64|aarch64)
    goarch=arm64
    ;;
  amd64|x86_64)
    goarch=amd64
    ;;
  *)
    echo "unsupported Docker server architecture" >&2
    exit 1
    ;;
esac

if ! docker image inspect "${image}" >/dev/null 2>&1; then
  docker pull "${image}" >/dev/null
fi

mkdir -p "${build_dir}"
echo "Building host manager and Linux ${goarch} fake inference binary in ./build"
go build -o "${build_dir}/llama-swap" .
CGO_ENABLED=0 GOOS=linux GOARCH="${goarch}" go build -o "${build_dir}/fake-model" ./cmd/fake-model

next_port() {
  python3 -c 'import socket; sock = socket.socket(); sock.bind(("127.0.0.1", 0)); print(sock.getsockname()[1]); sock.close()'
}

readonly server_port="$(next_port)"
readonly model_port="$(next_port)"
container_name="llama-swap-managed-container-it-$$"
readonly base_url="http://127.0.0.1:${server_port}"

show_logs() {
  echo "--- llama-swap integration log ---" >&2
  sed -n '1,240p' "${temp_dir}/llama-swap.log" >&2 || true
}

wait_for_health() {
  local attempt
  for attempt in $(seq 1 60); do
    if curl --fail --silent --show-error "${base_url}/health" >/dev/null; then
      return 0
    fi
    sleep 1
  done
  show_logs
  echo "llama-swap did not become healthy" >&2
  return 1
}

echo "Starting llama-swap control plane for digest-pinned Docker runtime"
RUNTIME_CONTAINER_IT_ROOT="${temp_dir}" \
RUNTIME_CONTAINER_IT_FAKE_MODEL="${build_dir}/fake-model" \
RUNTIME_CONTAINER_IT_MODEL_PORT="${model_port}" \
RUNTIME_CONTAINER_IT_NAME="${container_name}" \
  "${build_dir}/llama-swap" \
    -config "${fixture_dir}/config.yaml" \
    -listen "127.0.0.1:${server_port}" \
    >"${temp_dir}/llama-swap.log" 2>&1 &
server_pid="$!"
wait_for_health

echo "Staging and activating the real local Docker image"
curl --fail-with-body --silent --show-error --max-time 120 \
  -H "Authorization: Bearer ${api_key}" \
  -H 'Content-Type: application/json' \
  --data-binary "@${fixture_dir}/stage-vllm-container.json" \
  "${base_url}/api/runtimes/vllm-oci/stage" >/dev/null
curl --fail-with-body --silent --show-error --max-time 120 \
  -X POST -H "Authorization: Bearer ${api_key}" \
  "${base_url}/api/runtimes/vllm-oci/activate/${runtime_version}" >/dev/null

runtime_digest="$(curl --fail-with-body --silent --show-error \
  -H "Authorization: Bearer ${api_key}" \
  "${base_url}/api/runtimes/vllm-oci" | python3 -c '
import json
import sys

detail = json.load(sys.stdin)
assert detail["status"]["current"] == "container-it-v1", detail
manifest = detail["versions"]["container-it-v1"]
digest = manifest["metadata"]["imageDigest"]
assert digest.startswith("sha256:") and len(digest) == 71, manifest
print(digest)
')"

echo "Driving the digest-pinned managed container through Python OpenAI SDK"
BASE_URL="${base_url}" API_KEY="${api_key}" python3 - <<'PY'
import os

from openai import OpenAI

client = OpenAI(base_url=os.environ["BASE_URL"] + "/v1", api_key=os.environ["API_KEY"])
response = client.chat.completions.create(
    model="managed-container",
    messages=[{"role": "user", "content": "managed container runtime"}],
)
assert response.choices and response.choices[0].message.content, response
print("OpenAI SDK Chat passed through managed Docker runtime")
PY

started_image="$(docker inspect --format '{{.Config.Image}}' "${container_name}")"
expected_image="docker.io/library/alpine:3.20@${runtime_digest}"
if [[ "${started_image}" != "${expected_image}" ]]; then
  echo "managed container image=${started_image}, want ${expected_image}" >&2
  show_logs
  exit 1
fi

curl --fail-with-body --silent --show-error --max-time 30 \
  -X POST -H "Authorization: Bearer ${api_key}" \
  "${base_url}/api/models/unload/managed-container" >/dev/null
for attempt in $(seq 1 30); do
  if ! docker inspect "${container_name}" >/dev/null 2>&1; then
    echo "Managed Docker runtime integration passed"
    exit 0
  fi
  sleep 1
done
echo "managed container did not stop after unload" >&2
show_logs
exit 1
