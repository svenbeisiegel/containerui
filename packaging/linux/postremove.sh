#!/bin/sh
# The unit file is gone, or replaced, so systemd reads its units again. The
# configuration in /etc/containerui and the compose projects in
# /var/lib/containerui are kept.
[ -d /run/systemd/system ] || exit 0
systemctl daemon-reload >/dev/null 2>&1 || :
exit 0
