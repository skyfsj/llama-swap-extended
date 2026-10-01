#!/bin/sh
set -eu

port=19090
while [ "$#" -gt 0 ]; do
  case "$1" in
    --port)
      port="$2"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done

exec /work/bin/fake-model --listen "127.0.0.1:${port}" --tokens 3 --tps 200
