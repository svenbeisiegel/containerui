#!/bin/sh
# dpkg calls postinst with configure and the version it upgrades from, empty on
# an install. The service is enabled and started on the install, and restarted
# into the new binary on an upgrade, unless it had been stopped. Without
# systemd running, in a container build for instance, there is nothing to start.
[ "$1" = configure ] || exit 0
echo "containerui: the web interface is at https://localhost:8443/ (listen address in /etc/containerui/config.yaml)"
if [ -z "$2" ]; then
    echo "containerui: sign in with a system account that may use the docker or podman socket (e.g. root or a member of the docker group)"
fi
[ -d /run/systemd/system ] || exit 0
systemctl daemon-reload >/dev/null 2>&1 || :
if [ -z "$2" ]; then
    systemctl enable --now containerui.service >/dev/null 2>&1 || :
else
    systemctl try-restart containerui.service >/dev/null 2>&1 || :
fi
exit 0
