#!/bin/bash
# Checks that the collector intervals added to the plugin (#276) use the
# daemon's defaults everywhere and reach the daemon: default.cfg, the
# first-install config in the .plg, the start script (fallback, validation
# and passthrough) and the settings page. Run by `make test-packaging`.
#
# Usage: bash scripts/test-plugin-intervals.sh

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MAIN="${REPO_ROOT}/main.go"
DEFAULT_CFG="${REPO_ROOT}/meta/plugin/default.cfg"
PLG="${REPO_ROOT}/unraid-management-agent.plg"
START="${REPO_ROOT}/meta/plugin/scripts/start"
PAGE="${REPO_ROOT}/meta/plugin/unraid-management-agent.page"

INTERVALS=(
    INTERVAL_FAN_CONTROL
    INTERVAL_TUNING
    INTERVAL_DOCKER_UPDATE
    INTERVAL_DOCKER_NETWORKS
    INTERVAL_PLUGIN_UPDATE
    INTERVAL_OS_UPDATE
    INTERVAL_MOVER
)

# Prints an error and exits non-zero.
fail() {
    echo "✗ $*" >&2
    exit 1
}

# Prints the daemon default of env var $1 from its main.go struct tag.
daemon_default() {
    sed -n "s/.*default:\"\([0-9]*\)\" env:\"$1\".*/\1/p" "$MAIN"
}

# Prints the quoted value of $1="..." in file $2 (first match).
cfg_value() {
    sed -n "s/^$1=\"\([^\"]*\)\"$/\1/p" "$2" | head -n 1
}

# The block of variables validated by sanitize_int and the env passthrough.
sanitized=$(sed -n '/^for var in INTERVAL_SYSTEM/,/; do$/p' "$START")
passthrough=$(sed -n '/^nohup env/,/^$/p' "$START")

for var in "${INTERVALS[@]}"; do
    want=$(daemon_default "$var")
    [ -n "$want" ] || fail "$var: no default in main.go"

    got=$(cfg_value "$var" "$DEFAULT_CFG")
    [ "$got" = "$want" ] || fail "$var: default.cfg has '$got', daemon default is '$want'"

    got=$(cfg_value "$var" "$PLG")
    [ "$got" = "$want" ] || fail "$var: .plg first-install config has '$got', daemon default is '$want'"

    got=$(sed -n "s/^$var=\"\${$var:-\([^}]*\)}\"$/\1/p" "$START")
    [ "$got" = "$want" ] || fail "$var: start script fallback is '$got', daemon default is '$want'"

    grep -qw "$var" <<<"$sanitized" || fail "$var: not validated with sanitize_int in the start script"
    grep -q "^  $var=\"\$$var\" \\\\$" <<<"$passthrough" || fail "$var: not passed to the daemon by the start script"

    got=$(sed -n "s/.*\$config\['$var'\] ?? '\([^']*\)'.*/\1/p" "$PAGE")
    [ "$got" = "$want" ] || fail "$var: settings page fallback is '$got', daemon default is '$want'"
    grep -q "<select name=\"$var\"" "$PAGE" || fail "$var: no select on the settings page"

    echo "✓ $var defaults to $want everywhere and reaches the daemon"
done

# sanitize_int keeps digits only, so a bad value cannot inject shell code.
eval "$(grep '^sanitize_int()' "$START")"
[ "$(sanitize_int '60')" = "60" ] || fail "sanitize_int changed a valid value"
[ "$(sanitize_int '5; rm -rf /')" = "5" ] || fail "sanitize_int kept non-digits"
echo "✓ sanitize_int strips everything but digits"
