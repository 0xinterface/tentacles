#!/bin/sh
# install-host-deps.sh — one-time host dependencies for tentacles (Debian).
#
# Installs the official GitHub Actions runner Linux dependencies for the
# pinned runner line (2.337.x) on Debian-family hosts, plus the bootstrap
# tools the host needs (curl, ca-certificates, jq, git).
#
# Idempotent: safe to re-run; apt-get skips already-installed packages.
# Requires root. Run as:
#
#     sudo ./scripts/install-host-deps.sh
set -eu

RUNNER_VERSION="${RUNNER_VERSION:-2.337.0}"

die() {
    echo "install-host-deps: error: $*" >&2
    exit 1
}

[ "$(id -u)" -eq 0 ] || die "must run as root (e.g. sudo ./install-host-deps.sh)"

# The libicu package name varies by distro release. Probe the primary
# candidate for this host, then fall back through the known set for the
# 2.337 runner line until apt-cache resolves one.
pick_icu() {
    # shellcheck disable=SC1091
    . /etc/os-release 2>/dev/null || true
    case "${ID:-}${VERSION_ID:-}" in
        debian13*) first=libicu76 ;;
        ubuntu24* | ubuntu23*) first=libicu74 ;;
        debian12*) first=libicu72 ;;
        ubuntu22*) first=libicu70 ;;
        debian11* | ubuntu21*) first=libicu67 ;;
        ubuntu20*) first=libicu66 ;;
        debian10*) first=libicu63 ;;
        *) first=libicu72 ;;
    esac
    for pkg in "$first" libicu76 libicu74 libicu72 libicu70 libicu67 libicu66 libicu63; do
        if apt-cache show "$pkg" >/dev/null 2>&1; then
            printf '%s\n' "$pkg"
            return 0
        fi
    done
    return 1
}

echo "==> Updating package index"
apt-get update

if ! LIBICU="$(pick_icu)"; then
    die "no libicu package found for this distro (tried: libicu76 libicu74 libicu72 libicu70 libicu67 libicu66 libicu63)"
fi
echo "==> Using $LIBICU for the runner's libicu dependency"

echo "==> Installing runner dependencies and bootstrap tools"
apt-get install -y --no-install-recommends \
    "$LIBICU" \
    libkrb5-3 \
    zlib1g \
    curl \
    ca-certificates \
    jq \
    git

echo
echo "==> Done"
echo "    libicu package : $LIBICU"
echo "    runner version : $RUNNER_VERSION (informational)"
echo
echo "    The extracted runner payload for v$RUNNER_VERSION also ships"
echo "    bin/installdependencies.sh (run once per host/version as root,"
echo "    e.g. sudo /var/lib/tentacles/template/bin/installdependencies.sh)."
echo "    It covers the same base deps; this script is the idempotent"
echo "    host-level alternative for the pinned 2.337 line."
echo
echo "    Next steps: create the unprivileged gha-runner user and the"
echo "    /etc/tentacles, /var/lib/tentacles, /var/cache/tentacles,"
echo "    /var/log/tentacles, /run/tentacles directories (see README)."
