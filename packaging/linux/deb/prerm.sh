#!/bin/sh
# dpkg calls prerm with remove when the package is removed and upgrade when it
# is upgraded. Only a removal stops and disables the service; an upgrade
# restarts it after.
[ -d /run/systemd/system ] || exit 0
if [ "$1" = remove ]; then
    systemctl disable --now containerui.service >/dev/null 2>&1 || :
fi
exit 0
