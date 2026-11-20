#!/bin/sh
# dpkg calls postinst with configure and the version it upgrades from, empty on
# an install. Everything but the binary lives in /var/lib/containerui. The
# configuration is not a conffile of the package: the install writes the
# default there, and an upgrade from a version that kept it in
# /etc/containerui moves it, the TLS certificate and key and any other file of
# that folder over. The service is enabled and started on the install, and
# restarted into the new binary on an upgrade, unless it had been stopped.
# Without systemd running, in a container build for instance, there is nothing
# to start.
[ "$1" = configure ] || exit 0
data=/var/lib/containerui
old=/etc/containerui
if [ ! -e "$data/config.yaml" ]; then
    if [ -f "$old/config.yaml" ]; then
        for f in "$old"/* "$old"/.[!.]*; do
            [ -f "$f" ] || continue
            case "$f" in *.dpkg-*|*.rpmnew|*.rpmsave|*.rpmorig) continue ;; esac
            [ -e "$data/${f##*/}" ] || mv "$f" "$data/"
        done
        rmdir "$old" 2>/dev/null || :
        echo "containerui: moved the configuration from $old to $data"
    else
        /usr/bin/containerui init-config "$data/config.yaml" >/dev/null 2>&1 ||
            echo "containerui: could not write $data/config.yaml" >&2
    fi
fi
echo "containerui: the web interface is at https://localhost:8443/ (listen address in $data/config.yaml)"
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
