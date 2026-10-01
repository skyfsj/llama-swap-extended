#!/bin/bash
# Seed the managed-runtime shaped copy of the bundled llama.cpp server.
# Runs in the unified Dockerfile's variant targets after llama.cpp
# binaries have been copied; variants without llama.cpp never invoke it.
set -euo pipefail
if [ ! -x /usr/local/bin/llama-server ]; then
    echo "seed-llamacpp: llama-server missing, skipping" >&2
    exit 0
fi
mkdir -p /opt/llama-swap/runtimes/llamacpp/bin /opt/llama-swap/runtimes/llamacpp/build
cp /usr/local/bin/llama-server /opt/llama-swap/runtimes/llamacpp/bin/llama-server
cp /usr/local/bin/llama-cli /opt/llama-swap/runtimes/llamacpp/bin/llama-cli
cp /usr/local/bin/llama-tts /opt/llama-swap/runtimes/llamacpp/bin/llama-tts
cp /usr/local/bin/llama-bench /opt/llama-swap/runtimes/llamacpp/bin/llama-bench
printf '%s\n' '{"mode":"native","sourceType":"bundled","builtin":"true"}' \
    > /opt/llama-swap/runtimes/llamacpp/metadata.json
