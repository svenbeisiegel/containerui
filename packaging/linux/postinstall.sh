#!/bin/sh
# rpm passes 1 on an install and 2 or more on an upgrade. The service is
# enabled and started on the install, and restarted into the new binary on an
# upgrade, unless it had been stopped. Without systemd running, in a container
# build for instance, there is nothing to start.
note() {
    echo "containerui: the web interface is at https://localhost:8443/ (listen address in /etc/containerui/config.yaml)"
    if [ "${1:-1}" -eq 1 ]; then
        echo "containerui: sign in with a system account that may use the docker or podman socket (e.g. root or a member of the docker group)"
    fi
}
if [ -d /run/systemd/system ]; then
    systemctl daemon-reload >/dev/null 2>&1 || :
    if [ "${1:-1}" -eq 1 ]; then
        systemctl enable --now containerui.service >/dev/null 2>&1 || :
    else
        systemctl try-restart containerui.service >/dev/null 2>&1 || :
    fi
fi
note "${1:-1}"
exit 0
