#!/bin/sh
set -e

# ── Start sfh ────────────────────────────────────────────────────────────────
SFH_ARGS="--port ${PORT} --dir ${DIR}"
if [ -n "${AUTH}" ]; then
    SFH_ARGS="${SFH_ARGS} --auth ${AUTH}"
fi

echo "[sfh] starting on port ${PORT}, dir=${DIR}"
sfh ${SFH_ARGS} &
SFH_PID=$!

# ── Optionally start cloudflared tunnel ──────────────────────────────────────
CF_PID=""
if [ "${CF}" = "true" ]; then
    if [ -z "${TOKEN}" ]; then
        echo "[cloudflared] CF=true but CF_TOKEN is not set — tunnel skipped" >&2
    else
        echo "[cloudflared] starting tunnel"
        cloudflared tunnel --no-autoupdate run --token "${TOKEN}" &
        CF_PID=$!
    fi
fi

# ── Wait: exit if either process dies ────────────────────────────────────────
wait_any() {
    while true; do
        # sfh exited
        if ! kill -0 "${SFH_PID}" 2>/dev/null; then
            echo "[entrypoint] sfh exited, shutting down" >&2
            [ -n "${CF_PID}" ] && kill "${CF_PID}" 2>/dev/null
            exit 1
        fi
        # cloudflared exited (only when it was started)
        if [ -n "${CF_PID}" ] && ! kill -0 "${CF_PID}" 2>/dev/null; then
            echo "[entrypoint] cloudflared exited, shutting down" >&2
            kill "${SFH_PID}" 2>/dev/null
            exit 1
        fi
        sleep 2
    done
}

wait_any
