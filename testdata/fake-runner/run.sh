#!/bin/sh
# fake-runner: stub of the official actions/runner run.sh for tentacles
# tests. It consumes --jitconfig like the real runner, proves the JIT value
# reached it by writing a marker file, then either sleeps a fixed time or
# waits for a signal.
#
# Environment:
#   FAKE_RUNNER_MARKER     marker path (default: $PWD/.fake-claimed)
#   FAKE_RUNNER_SLEEP      seconds to run before exiting on its own
#   FAKE_RUNNER_EXIT_CODE  exit code when leaving (default: 0)
set -u

jit=""
while [ "$#" -gt 0 ]; do
    case "$1" in
        --jitconfig=*)
            jit="${1#--jitconfig=}"
            shift
            ;;
        --jitconfig)
            shift
            if [ "$#" -gt 0 ]; then
                jit="$1"
            fi
            shift
            ;;
        *)
            shift
            ;;
    esac
done

if [ -z "$jit" ]; then
    echo "fake-runner: no --jitconfig value provided" >&2
    exit 2
fi

marker="${FAKE_RUNNER_MARKER:-$PWD/.fake-claimed}"
if command -v sha256sum >/dev/null 2>&1; then
    digest=$(printf '%s' "$jit" | sha256sum | cut -d' ' -f1)
elif command -v shasum >/dev/null 2>&1; then
    digest=$(printf '%s' "$jit" | shasum -a 256 | cut -d' ' -f1)
else
    echo "fake-runner: no sha256 utility available" >&2
    exit 2
fi
printf '%s %s\n' "$digest" "$$" > "$marker"
echo "fake runner listening"

if [ -n "${FAKE_RUNNER_SLEEP:-}" ]; then
    sleep "$FAKE_RUNNER_SLEEP"
    exit "${FAKE_RUNNER_EXIT_CODE:-0}"
fi

trap 'exit "${FAKE_RUNNER_EXIT_CODE:-0}"' TERM INT
while :; do
    sleep 3600 &
    wait "$!"
done
