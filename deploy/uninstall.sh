#!/usr/bin/env bash
# Remove the ElVa service. The settings file is kept unless --purge is given.
# The database is never touched.
#
#   sudo deploy/uninstall.sh            # remove service and binary, keep PREFIX/elva.env
#   sudo deploy/uninstall.sh --purge    # also remove PREFIX and the elva user
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
    echo "purged $PREFIX and user elva"
else
    [[ -e $PREFIX/elva.env ]] && echo "kept $PREFIX/elva.env (use --purge to remove it)"
fi
echo "The database was not touched."
