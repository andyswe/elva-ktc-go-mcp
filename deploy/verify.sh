#!/usr/bin/env bash
# Check an ElVa installation: files, configuration, service state, a live check of
# the RCU and the database, and recent warnings. Exits 1 if anything failed.
#
#   sudo deploy/verify.sh
#
# The live check runs "elva -check" as the service user with the installed settings.
# It logs in to the RCU and does one read, which restores the RCU cursor like any scrape.
set -uo pipefail

PREFIX=${PREFIX:-/opt/elva}
REPO=$(cd "$(dirname "$0")/.." && pwd)
UNIT=/etc/systemd/system/elva.service
BIN=$PREFIX/bin/elva
ENV=$PREFIX/elva.env

fails=0 warns=0
# Colors only on a terminal, so output piped to a file or mail stays clean.
if [[ -t 1 ]]; then G=$'\033[32m' Y=$'\033[33m' R=$'\033[31m' N=$'\033[0m'; else G= Y= R= N=; fi
ok()   { printf '  %sOK%s    %s\n' "$G" "$N" "$*"; }
warn() { printf '  %sWARN%s  %s\n' "$Y" "$N" "$*"; warns=$((warns + 1)); }
fail() { printf '  %sFAIL%s  %s\n' "$R" "$N" "$*"; fails=$((fails + 1)); }
section() { printf '\n%s\n' "$*"; }

# setting KEY: value of KEY in the settings file ("" if unset or commented out).
setting() { sed -n "s/^[[:space:]]*\(export[[:space:]]\+\)\?$1=//p" "$ENV" | tail -n1 | sed "s/^[\"']//; s/[\"']\$//"; }

[[ $EUID -eq 0 ]] || { echo "verify.sh: must run as root to read the settings: sudo $0" >&2; exit 2; }

section "Installation ($PREFIX)"
if [[ -x $BIN ]]; then
    installed=$("$BIN" -version 2>/dev/null) && ok "binary $BIN, version $installed" || fail "binary $BIN does not run"
    [[ $(stat -c %U "$BIN") == root ]] && ok "binary owned by root" || fail "binary not owned by root: $(stat -c %U:%G "$BIN")"
    if [[ -x $REPO/bin/elva ]]; then
        built=$("$REPO/bin/elva" -version 2>/dev/null)
        [[ $built == "$installed" ]] && ok "matches the build in $REPO/bin ($built)" \
            || warn "installed $installed differs from built $built: run sudo deploy/install.sh to upgrade"
    fi
else
    fail "no binary at $BIN: run scripts/build.sh and sudo deploy/install.sh"
fi
id elva >/dev/null 2>&1 && ok "system user elva exists" || fail "system user elva missing"

section "Configuration ($ENV)"
if [[ -f $ENV ]]; then
    perms=$(stat -c '%a %U:%G' "$ENV")
    [[ $perms == "640 root:elva" ]] && ok "permissions $perms" || fail "permissions $perms, expected 640 root:elva (contains passwords)"
    host=$(setting KTC_HOST) pw=$(setting KTC_PASSWORD) db=$(setting ELVA_DATABASE_URL)
    [[ -n $host ]] && ok "KTC_HOST=$host" || fail "KTC_HOST not set"
    if [[ -z $pw ]]; then fail "KTC_PASSWORD not set"
    elif [[ $pw == changeme ]]; then fail "KTC_PASSWORD is still the sample value"
    else ok "KTC_PASSWORD set"; fi
    if [[ -z $db ]]; then warn "ELVA_DATABASE_URL not set: readings only go to the journal, nothing is stored"
    elif [[ $db == *changeme* ]]; then fail "ELVA_DATABASE_URL still has the sample password"
    else ok "ELVA_DATABASE_URL set"; fi
else
    fail "no settings file at $ENV"
fi

section "Service (elva.service)"
if [[ -f $UNIT ]]; then
    ok "unit installed: $UNIT"
    if [[ -f $REPO/deploy/elva.service ]]; then
        if diff -q <(sed "s|@PREFIX@|$PREFIX|g" "$REPO/deploy/elva.service") "$UNIT" >/dev/null; then
            ok "unit matches deploy/elva.service"
        else
            warn "unit differs from deploy/elva.service: run sudo deploy/install.sh"
        fi
    fi
    [[ $(systemctl is-enabled elva 2>/dev/null) == enabled ]] && ok "enabled (starts at boot)" || fail "not enabled: sudo systemctl enable elva"
    state=$(systemctl is-active elva 2>/dev/null)
    since=$(systemctl show elva -p ActiveEnterTimestamp --value)
    restarts=$(systemctl show elva -p NRestarts --value)
    case $state in
        active)     ok "running since $since" ;;
        activating) warn "starting or waiting to restart (restarts so far: $restarts); see journalctl -u elva" ;;
        *)          fail "not running ($state, last exit status $(systemctl show elva -p ExecMainStatus --value)): see journalctl -u elva" ;;
    esac
    [[ ${restarts:-0} -gt 0 ]] && warn "restarted $restarts time(s) since it was started"
else
    fail "unit not installed: run sudo deploy/install.sh"
fi

section "Live check (elva -check as user elva)"
if [[ -x $BIN && -f $ENV ]] && id elva >/dev/null 2>&1; then
    out=$(runuser -u elva -- env -i PATH=/usr/bin:/bin "$BIN" -env-file "$ENV" -check 2>&1)
    code=$?
    grep -v 'msg=setting' <<<"$out" | sed 's/^time=[^ ]* /    /'
    [[ $code -eq 0 ]] && ok "configuration, RCU and database checks passed" || fail "elva -check failed with exit code $code"
    # The service writes a row every minute; older than 5 minutes means it isn't.
    if age=$(grep -o 'age_seconds=[0-9]*' <<<"$out"); then
        age=${age#age_seconds=}
        (( age < 300 )) && ok "data is being stored (last row ${age}s ago)" \
            || warn "last stored row is ${age}s old: is the service storing data?"
    fi
else
    fail "skipped: binary, settings or user missing"
fi

section "Recent warnings (last hour)"
recent=$(journalctl -u elva --since "-1h" -p warning --no-pager -o short-iso 2>/dev/null | grep -v '^-- ' | tail -n 10)
if [[ -n $recent ]]; then
    warn "journal has warnings or errors in the last hour:"
    sed 's/^/    /' <<<"$recent"
else
    ok "no warnings or errors in the last hour"
fi

echo
if (( fails > 0 )); then
    echo "Result: $fails problem(s), $warns warning(s)"; exit 1
fi
echo "Result: all good ($warns warning(s))"
