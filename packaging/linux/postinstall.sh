#!/bin/sh
# rpm passes 1 on an install and 2 or more on an upgrade. Everything but the
# binary lives in /var/lib/containerui. The configuration is not a file of the
# package: the install writes the default there, and an upgrade from a version
# that kept it in /etc/containerui moves it, the TLS certificate and key and
# any other file of that folder over; the old package's removal, which follows
# in the same transaction, then takes the empty folder away. The service is
# enabled and started on the install, and restarted into the new binary on an
# upgrade, unless it had been stopped. Without systemd running, in a container
# build for instance, there is nothing to start.
data=/var/lib/containerui
old=/etc/containerui
setup_config() {
    [ -e "$data/config.yaml" ] && return 0
    if [ -f "$old/config.yaml" ]; then
        for f in "$old"/* "$old"/.[!.]*; do
            [ -f "$f" ] || continue
            case "$f" in *.rpmnew|*.rpmsave|*.rpmorig|*.dpkg-*) continue ;; esac
            [ -e "$data/${f##*/}" ] || mv "$f" "$data/"
        done
        echo "containerui: moved the configuration from $old to $data"
    else
        /usr/bin/containerui init-config "$data/config.yaml" >/dev/null 2>&1 ||
            echo "containerui: could not write $data/config.yaml" >&2
    fi
}
note() {
    echo "containerui: the web interface is at https://localhost:8443/ (listen address in $data/config.yaml)"
    if [ "${1:-1}" -eq 1 ]; then
        echo "containerui: sign in with a system account that may use the docker or podman socket (e.g. root or a member of the docker group)"
    fi
}
setup_config
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
