#!/usr/bin/env bash
# Install or upgrade ElVa as a systemd service. Safe to run again.
#
#   scripts/build.sh                     # as a normal user
#   sudo deploy/install.sh [BINARY]      # default BINARY: bin/elva
#
# Installs to PREFIX (default /opt/elva):
#   PREFIX/bin/elva   binary (root-owned)
#   PREFIX/elva.env   settings (0640 root:elva); created from .env.sample, never overwritten
# plus /etc/systemd/system/elva.service and a system user "elva".
set -euo pipefail

PREFIX=${PREFIX:-/opt/elva}
REPO=$(cd "$(dirname "$0")/.." && pwd)
BIN=${1:-$REPO/bin/elva}
UNIT=/etc/systemd/system/elva.service

die() { echo "install.sh: $*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "must run as root: sudo $0${*:+ $*}"
command -v systemctl >/dev/null || die "systemd not found"
[[ -x $BIN ]] || die "no binary at $BIN; run scripts/build.sh first"
VERSION=$("$BIN" -version) || die "$BIN does not run on this machine"
[[ $PREFIX == /home/* ]] && die "PREFIX under /home is hidden by the unit's ProtectHome=yes"

echo "Installing ElVa $VERSION to $PREFIX"

if ! id elva >/dev/null 2>&1; then
    useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin elva
    echo "  created system user elva"
fi

install -d -m 0755 -o root -g root "$PREFIX" "$PREFIX/bin"
# Copy then rename, so a running service never sees a half-written binary.
install -m 0755 -o root -g root "$BIN" "$PREFIX/bin/elva.new"
mv -f "$PREFIX/bin/elva.new" "$PREFIX/bin/elva"
echo "  installed $PREFIX/bin/elva"

first=no
if [[ ! -e $PREFIX/elva.env ]]; then
    install -m 0640 -o root -g elva "$REPO/.env.sample" "$PREFIX/elva.env"
    first=yes
    echo "  created $PREFIX/elva.env from .env.sample"
else
    # Keep the settings, but make sure only root and the service can read them.
    chown root:elva "$PREFIX/elva.env"
    chmod 0640 "$PREFIX/elva.env"
    echo "  kept existing $PREFIX/elva.env"
fi

sed "s|@PREFIX@|$PREFIX|g" "$REPO/deploy/elva.service" > "$UNIT.new"
chmod 0644 "$UNIT.new"
mv -f "$UNIT.new" "$UNIT"
systemctl daemon-reload
systemctl enable elva.service >/dev/null 2>&1
echo "  installed and enabled $UNIT"

if [[ $first == yes ]]; then
    cat <<EOF

First install: fill in the settings, then start the service:
  sudo nano $PREFIX/elva.env
  sudo systemctl start elva
  sudo deploy/verify.sh
EOF
    exit 0
fi

systemctl restart elva.service
echo "  restarted elva"
sleep 3
echo
systemctl --no-pager --lines=0 status elva.service || true
echo
journalctl -u elva.service -n 12 --no-pager -o cat || true
echo
echo "Check everything with: sudo deploy/verify.sh"
