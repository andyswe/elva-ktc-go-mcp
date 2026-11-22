#!/usr/bin/env bash
# Remove the ElVa service. The settings file is kept unless --purge is given.
# The database is never touched.
#
#   sudo deploy/uninstall.sh            # remove service and binaries, keep the *.env files
#   sudo deploy/uninstall.sh --purge    # also remove PREFIX, the elva user and the elva-mcp group
set -euo pipefail

PREFIX=${PREFIX:-/opt/elva}
UNIT=/etc/systemd/system/elva.service
purge=no
[[ ${1:-} == --purge ]] && purge=yes

[[ $EUID -eq 0 ]] || { echo "uninstall.sh: must run as root: sudo $0${*:+ $*}" >&2; exit 1; }

if [[ -e $UNIT ]]; then
    systemctl disable --now elva.service >/dev/null 2>&1 || true
    rm -f "$UNIT"
    systemctl daemon-reload
    systemctl reset-failed elva.service >/dev/null 2>&1 || true
    echo "removed service"
fi
rm -rf "$PREFIX/bin"
echo "removed $PREFIX/bin"

if [[ $purge == yes ]]; then
    rm -rf "$PREFIX"
    id elva >/dev/null 2>&1 && userdel elva
    getent group elva-mcp >/dev/null && groupdel elva-mcp
    echo "purged $PREFIX, user elva and group elva-mcp"
else
    for f in elva.env mcp.env; do
        [[ -e $PREFIX/$f ]] && echo "kept $PREFIX/$f (use --purge to remove it)"
    done
fi
echo "The database was not touched."
