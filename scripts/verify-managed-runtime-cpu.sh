#!/usr/bin/env bash
set -euo pipefail

# Exercises the Linux-only managed-runtime providers without requiring a GPU:
# - builds llama.cpp from its real upstream source through Runtime API stage;
# - activates it and verifies the canonical current/bin/llama-server path;
# - stages a tiny local Python package through the real uv VLLM provider;
# - starts a managed llama.cpp simulator and drives Chat/Responses through the
#   Python OpenAI SDK, including both streaming variants.

readonly repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly fixture_dir="${repo_root}/test/integration/managed-runtime-cpu"
readonly manager_binary="${LLAMA_SWAP_RUNTIME_TEST_BINARY:-}"
readonly image="${LLAMA_SWAP_RUNTIME_TEST_IMAGE:-alpine:3.20}"
readonly uv_image="${LLAMA_SWAP_RUNTIME_TEST_UV_IMAGE:-ghcr.io/astral-sh/uv:0.12.7-alpine}"
readonly apk_repository="${LLAMA_SWAP_RUNTIME_TEST_APK_REPOSITORY:-https://mirrors.aliyun.com/alpine/v3.20}"
readonly llama_cpp_source_path="${LLAMA_CPP_SOURCE_PATH:-}"
readonly skip_llama="${LLAMA_SWAP_RUNTIME_TEST_SKIP_LLAMA:-0}"
readonly skip_tools="${LLAMA_SWAP_RUNTIME_TEST_SKIP_TOOLS:-0}"
readonly skip_vllm="${LLAMA_SWAP_RUNTIME_TEST_SKIP_VLLM:-0}"
readonly container="llama-swap-managed-runtime-it-$$"
readonly api_key="managed-runtime-test-key"
readonly temp_dir="$(mktemp -d /tmp/llama-swap-managed-runtime.XXXXXX)"
readonly build_dir="${repo_root}/build/managed-runtime-cpu-it-$$"
uv_container=""

cleanup() {
  if [[ -n "${uv_container}" ]]; then
    docker rm --force "${uv_container}" >/dev/null 2>&1 || true
  fi
  docker rm --force "${container}" >/dev/null 2>&1 || true
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

mkdir -p "${build_dir}"
if [[ -n "${manager_binary}" ]]; then
  if [[ ! -f "${manager_binary}" || ! -x "${manager_binary}" ]]; then
    echo "LLAMA_SWAP_RUNTIME_TEST_BINARY is not an executable file: ${manager_binary}" >&2
    exit 1
  fi
  echo "Using explicit prebuilt Linux manager binary from ${manager_binary}"
  cp "${manager_binary}" "${build_dir}/llama-swap"
else
  echo "Cross-building Linux ${goarch} test binaries in ./build"
  CGO_ENABLED=0 GOOS=linux GOARCH="${goarch}" go build -o "${build_dir}/llama-swap" .
fi
CGO_ENABLED=0 GOOS=linux GOARCH="${goarch}" go build -o "${build_dir}/fake-model" ./cmd/fake-model
if [[ "${skip_vllm}" != "1" ]]; then
  uv_container="llama-swap-managed-runtime-uv-it-$$"
  if ! docker image inspect "${uv_image}" >/dev/null 2>&1; then
    docker pull "${uv_image}" >/dev/null
  fi
  docker create --name "${uv_container}" "${uv_image}" >/dev/null
  docker cp "${uv_container}:/usr/local/bin/uv" "${build_dir}/uv"
  chmod 0755 "${build_dir}/uv"
fi

show_logs() {
  echo "--- llama-swap integration log ---" >&2
  docker exec "${container}" tail -n 200 /work/llama-swap.log >&2 || true
}

wait_for_health() {
  local health_url="$1"
  local attempt
  for attempt in $(seq 1 90); do
    if curl --fail --silent --show-error "${health_url}/health" >/dev/null; then
      return 0
    fi
    sleep 1
  done
  show_logs
  echo "llama-swap did not become healthy" >&2
  return 1
}

echo "Starting Linux CPU integration container from ${image}"
runtime_mounts=(
  --volume "${repo_root}:/workspace:ro"
  --volume "${temp_dir}:/work"
  --volume "${build_dir}:/work/bin:ro"
)
if [[ -n "${llama_cpp_source_path}" ]]; then
  if [[ ! -d "${llama_cpp_source_path}" ]]; then
    echo "LLAMA_CPP_SOURCE_PATH is not a directory: ${llama_cpp_source_path}" >&2
    exit 1
  fi
  runtime_mounts+=(--volume "${llama_cpp_source_path}:/work/llama.cpp-source:ro")
fi
docker run --detach --rm --name "${container}" \
  --publish 127.0.0.1::18080 \
  "${runtime_mounts[@]}" \
  "${image}" sleep infinity >/dev/null

echo "Installing CPU build tools and uv"
docker exec --env "APK_REPOSITORY=${apk_repository}" \
  --env "SKIP_LLAMA=${skip_llama}" --env "SKIP_TOOLS=${skip_tools}" \
  --env "SKIP_VLLM=${skip_vllm}" \
  "${container}" sh -ceu '
  if [ "$SKIP_TOOLS" != "1" ] && command -v apk >/dev/null 2>&1; then
    printf "%s/main\n%s/community\n" "$APK_REPOSITORY" "$APK_REPOSITORY" >/etc/apk/repositories
    apk_packages="build-base ca-certificates cmake"
    if [ "$SKIP_LLAMA" != "1" ] || [ "$SKIP_VLLM" != "1" ]; then
      apk_packages="$apk_packages git"
    fi
    if [ "$SKIP_VLLM" != "1" ]; then
      apk_packages="$apk_packages curl py3-pip python3"
    fi
    # shellcheck disable=SC2086
    apk add --no-cache $apk_packages
  elif [ "$SKIP_TOOLS" != "1" ] && command -v apt-get >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -o Acquire::Retries=3
    apt_packages="build-essential ca-certificates cmake"
    if [ "$SKIP_LLAMA" != "1" ] || [ "$SKIP_VLLM" != "1" ]; then
      apt_packages="$apt_packages git"
    fi
    if [ "$SKIP_VLLM" != "1" ]; then
      apt_packages="$apt_packages curl python3 python3-pip python3-venv"
    fi
    # shellcheck disable=SC2086
    apt-get install -y --no-install-recommends $apt_packages
    rm -rf /var/lib/apt/lists/*
  elif [ "$SKIP_TOOLS" != "1" ]; then
    echo "CPU integration image must provide apk or apt-get" >&2
    exit 1
  fi
  if [ "$SKIP_VLLM" != "1" ]; then
    /work/bin/uv --version
    rm -rf /work/vllm-dist /work/fake-vllm-source /work/fake-vllm.git
    mkdir -p /work/vllm-dist
    cp -a /workspace/test/integration/managed-runtime-cpu/fake-vllm /work/fake-vllm-source
    python3 -m pip wheel --no-deps --no-build-isolation \
      --wheel-dir /work/vllm-dist \
      /work/fake-vllm-source
    git -C /work/fake-vllm-source init --quiet
    git -C /work/fake-vllm-source config user.email runtime-test@example.invalid
    git -C /work/fake-vllm-source config user.name runtime-test
    git -C /work/fake-vllm-source add .
    git -C /work/fake-vllm-source commit --quiet -m fixture
    git clone --bare --quiet /work/fake-vllm-source /work/fake-vllm.git
    git -C /work/fake-vllm-source rev-parse HEAD >/work/fake-vllm.commit
  fi
  mkdir -p /work/data /work/simulator/bin /work/simulator/build
  cp /workspace/test/integration/managed-runtime-cpu/llama-server-wrapper.sh /work/simulator/bin/llama-server
  cp /workspace/test/integration/managed-runtime-cpu/simulator-metadata.json /work/simulator/metadata.json
  chmod 0755 /work/simulator/bin/llama-server
  if [ "$SKIP_LLAMA" != "1" ]; then
    if [ -d /work/llama.cpp-source ]; then
      cp -a /work/llama.cpp-source /work/llama.cpp
    else
      git clone --depth 1 https://github.com/ggml-org/llama.cpp.git /work/llama.cpp
    fi
    git -C /work/llama.cpp rev-parse HEAD >/work/llama.cpp.commit
    git clone --bare /work/llama.cpp /work/llama.cpp.git
  fi
'

docker exec -d "${container}" sh -ceu '
  export PATH="/work/bin:${PATH}"
  exec /work/bin/llama-swap \
    -config /workspace/test/integration/managed-runtime-cpu/config.yaml \
    -listen :18080 \
    >/work/llama-swap.log 2>&1
'

readonly published_port="$(docker port "${container}" 18080/tcp | sed -E 's/.*:([0-9]+)$/\1/')"
readonly base_url="http://127.0.0.1:${published_port}"
wait_for_health "${base_url}"

stage_and_activate_vllm() {
  local runtime_name="$1"
  local request_path="$2"

  if ! curl --fail-with-body --silent --show-error --max-time 300 \
    -H "Authorization: Bearer ${api_key}" \
    -H 'Content-Type: application/json' \
    --data-binary "@${request_path}" \
    "${base_url}/api/runtimes/${runtime_name}/stage" >"${temp_dir}/${runtime_name}-stage.json"; then
    echo "${runtime_name} stage response:" >&2
    sed -n '1,120p' "${temp_dir}/${runtime_name}-stage.json" >&2 || true
    show_logs
    return 1
  fi
  curl --fail-with-body --silent --show-error --max-time 120 \
    -X POST -H "Authorization: Bearer ${api_key}" \
    "${base_url}/api/runtimes/${runtime_name}/activate/0.0.1" >/dev/null
  local actual_version
  actual_version="$(docker exec "${container}" "/work/data/runtimes/${runtime_name}/current/.venv/bin/vllm" --version)"
  if [[ "${actual_version}" != "fake-vllm 0.0.1" ]]; then
    echo "${runtime_name} vllm version=${actual_version}, want fake-vllm 0.0.1" >&2
    show_logs
    return 1
  fi
}

if [[ "${skip_llama}" != "1" ]]; then
  echo "Staging and activating actual llama.cpp CPU runtime from Git"
  readonly llama_cpp_commit="$(docker exec "${container}" cat /work/llama.cpp.commit)"
  readonly llama_stage_request="${temp_dir}/stage-llama-cpu-git.json"
  printf '{"kind":"llamacpp","mode":"native","version":"cpu-it-git-v1","sourceType":"git","source":"file:///work/llama.cpp.git","ref":"%s","build":{"driver":"cmake"}}\n' "${llama_cpp_commit}" >"${llama_stage_request}"
  if ! curl --fail-with-body --silent --show-error --max-time 900 \
    -H "Authorization: Bearer ${api_key}" \
    -H 'Content-Type: application/json' \
    --data-binary "@${llama_stage_request}" \
    "${base_url}/api/runtimes/llama-cpu/stage" >"${temp_dir}/llama-stage.json"; then
    echo "llama.cpp stage response:" >&2
    sed -n '1,120p' "${temp_dir}/llama-stage.json" >&2 || true
    show_logs
    exit 1
  fi
  curl --fail-with-body --silent --show-error --max-time 120 \
    -X POST -H "Authorization: Bearer ${api_key}" \
    "${base_url}/api/runtimes/llama-cpu/activate/cpu-it-git-v1" >/dev/null
  docker exec --env 'LD_LIBRARY_PATH=/work/data/runtimes/llama-cpu/current/bin:/work/data/runtimes/llama-cpu/current/build/bin' \
    "${container}" /work/data/runtimes/llama-cpu/current/bin/llama-server --version
else
  echo "Skipping llama.cpp stage (LLAMA_SWAP_RUNTIME_TEST_SKIP_LLAMA=1)"
fi

if [[ "${skip_vllm}" != "1" ]]; then
  echo "Staging and activating uv-managed vLLM fixture through local, wheel, Git, and offline PyPI paths"
  readonly vllm_wheel="/work/vllm-dist/vllm-0.0.1-py3-none-any.whl"
  readonly vllm_git_commit="$(docker exec "${container}" cat /work/fake-vllm.commit)"
  readonly vllm_wheel_stage_request="${temp_dir}/stage-vllm-wheel.json"
  readonly vllm_git_stage_request="${temp_dir}/stage-vllm-git.json"
  readonly vllm_pypi_stage_request="${temp_dir}/stage-vllm-pypi.json"
  printf '{"kind":"vllm","mode":"native","version":"0.0.1","sourceType":"wheel","source":"%s"}\n' "${vllm_wheel}" >"${vllm_wheel_stage_request}"
  printf '{"kind":"vllm","mode":"native","version":"0.0.1","sourceType":"git","source":"file:///work/fake-vllm.git","ref":"%s"}\n' "${vllm_git_commit}" >"${vllm_git_stage_request}"
  printf '%s\n' '{"kind":"vllm","mode":"native","version":"0.0.1","sourceType":"pypi","installArgs":["--no-index","--find-links","/work/vllm-dist"]}' >"${vllm_pypi_stage_request}"
  stage_and_activate_vllm vllm-cpu "${fixture_dir}/stage-vllm-cpu.json"
  stage_and_activate_vllm vllm-wheel "${vllm_wheel_stage_request}"
  stage_and_activate_vllm vllm-git "${vllm_git_stage_request}"
  stage_and_activate_vllm vllm-pypi "${vllm_pypi_stage_request}"
else
  echo "Skipping vLLM stage (LLAMA_SWAP_RUNTIME_TEST_SKIP_VLLM=1)"
fi

echo "Driving managed simulator through Python OpenAI SDK"
BASE_URL="${base_url}" API_KEY="${api_key}" python3 - <<'PY'
import os

from openai import OpenAI

client = OpenAI(base_url=os.environ["BASE_URL"] + "/v1", api_key=os.environ["API_KEY"])

chat = client.chat.completions.create(
    model="simulated",
    messages=[{"role": "user", "content": "hello"}],
)
assert chat.choices and chat.choices[0].message.content, chat

chat_chunks = list(client.chat.completions.create(
    model="simulated",
    messages=[{"role": "user", "content": "stream"}],
    stream=True,
))
assert any(chunk.choices and chunk.choices[0].delta.content for chunk in chat_chunks), chat_chunks

response = client.responses.create(model="simulated", input="responses")
assert response.output_text, response

response_events = list(client.responses.create(model="simulated", input="responses stream", stream=True))
assert response_events, response_events
assert any(getattr(event, "type", "") == "response.completed" for event in response_events), response_events

print("OpenAI SDK Chat, Chat SSE, Responses, and Responses SSE passed")
PY

curl --fail-with-body --silent --show-error \
  -H "Authorization: Bearer ${api_key}" \
  "${base_url}/api/runtimes" >"${temp_dir}/runtimes.json"
if [[ "${skip_llama}" != "1" ]]; then
  grep -q '"current":"cpu-it-git-v1"' "${temp_dir}/runtimes.json"
fi
if [[ "${skip_vllm}" != "1" ]]; then
  for runtime_name in vllm-cpu vllm-wheel vllm-git vllm-pypi; do
    RUNTIME_NAME="${runtime_name}" RUNTIMES_JSON="${temp_dir}/runtimes.json" python3 - <<'PY'
import json
import os

with open(os.environ["RUNTIMES_JSON"], encoding="utf-8") as source:
    runtimes = json.load(source)
details = {detail["name"]: detail for detail in runtimes["data"]}
detail = details[os.environ["RUNTIME_NAME"]]
assert detail["current"] == "0.0.1", detail
PY
  done
fi

echo "Managed runtime CPU integration passed"
