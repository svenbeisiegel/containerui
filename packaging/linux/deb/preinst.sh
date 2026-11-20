#!/bin/sh
# dpkg calls preinst with upgrade and the old version before it unpacks the
# new one. Versions before 0.14 kept the configuration, the TLS certificate
# and key in /etc/containerui; they move to /var/lib/containerui here, before
# the unpack, so that dpkg finds the old folder empty and removes it. postinst
# does the same for a folder no package owns, and writes the default
# configuration on an install.
[ "$1" = upgrade ] || exit 0
data=/var/lib/containerui
old=/etc/containerui
[ -f "$old/config.yaml" ] && [ ! -e "$data/config.yaml" ] || exit 0
mkdir -p "$data" && chmod 0750 "$data"
for f in "$old"/* "$old"/.[!.]*; do
    [ -f "$f" ] || continue
    case "$f" in *.dpkg-*|*.rpmnew|*.rpmsave|*.rpmorig) continue ;; esac
    [ -e "$data/${f##*/}" ] || mv "$f" "$data/"
done
echo "containerui: moved the configuration from $old to $data"
exit 0
