#!/usr/bin/env bash
# build.sh builds the package repositories served on GitHub Pages into OUT:
#
#   rpm/        an rpm-md repository for zypper and dnf, repomd.xml signed
#   deb/        an apt repository, suite stable, component main, Release signed
#   containerui.asc, containerui.gpg   the public key, armored and binary
#
# It is rebuilt whole from the rpm and deb assets of the KEEP newest GitHub
# releases, so older versions drop out as new ones are released.
#
# Needs gh (GH_TOKEN), createrepo_c, apt-ftparchive and gpg with the signing
# key imported: GPG_KEY_ID names it, GPG_PASSPHRASE unlocks it if it has one.
# REPO_URL is where the site is served, by default the repository's Pages URL.
set -euo pipefail

out=${1:?usage: build.sh OUT}
keep=${KEEP:-5}
key=${GPG_KEY_ID:?GPG_KEY_ID names the signing key}
repo=${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is owner/name}
owner=${repo%%/*}
url=${REPO_URL:-https://${owner,,}.github.io/${repo#*/}}

sign() {
    gpg --batch --yes --local-user "$key" --pinentry-mode loopback \
        --passphrase-fd 0 "$@" <<<"${GPG_PASSPHRASE:-}"
}

rm -rf "$out"
mkdir -p "$out/rpm" "$out/deb/pool/main" "$out/download"

tags=$(gh release list --repo "$repo" --exclude-drafts --exclude-pre-releases \
    --limit "$keep" --json tagName --jq '.[].tagName')
[ -n "$tags" ] || { echo "no releases to publish" >&2; exit 1; }
for tag in $tags; do
    echo "adding $tag"
    gh release download "$tag" --repo "$repo" --dir "$out/download" \
        --pattern '*.rpm' --pattern '*.deb' || echo "$tag has no packages"
done
shopt -s nullglob
rpms=("$out"/download/*.rpm)
debs=("$out"/download/*.deb)
[ ${#rpms[@]} -gt 0 ] && mv "${rpms[@]}" "$out/rpm/"
[ ${#debs[@]} -gt 0 ] && mv "${debs[@]}" "$out/deb/pool/main/"
rmdir "$out/download"

gpg --armor --export "$key" > "$out/containerui.asc"
gpg --export "$key" > "$out/containerui.gpg"

# rpm-md, for zypper and dnf
createrepo_c --update "$out/rpm"
sign --armor --detach-sign --output "$out/rpm/repodata/repomd.xml.asc" "$out/rpm/repodata/repomd.xml"
cat > "$out/rpm/containerui.repo" <<EOF
[containerui]
name=ContainerUI
baseurl=$url/rpm
type=rpm-md
enabled=1
autorefresh=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=$url/containerui.asc
EOF

# apt; Packages lists the files relative to the repository root
(
    cd "$out/deb"
    for arch in amd64 arm64; do
        dir=dists/stable/main/binary-$arch
        mkdir -p "$dir"
        apt-ftparchive --arch "$arch" packages pool > "$dir/Packages"
        gzip -9kn "$dir/Packages"
    done
    apt-ftparchive \
        -o APT::FTPArchive::Release::Origin=ContainerUI \
        -o APT::FTPArchive::Release::Label=ContainerUI \
        -o APT::FTPArchive::Release::Suite=stable \
        -o APT::FTPArchive::Release::Codename=stable \
        -o APT::FTPArchive::Release::Architectures="amd64 arm64" \
        -o APT::FTPArchive::Release::Components=main \
        release dists/stable > Release
    mv Release dists/stable/Release
)
sign --clearsign --output "$out/deb/dists/stable/InRelease" "$out/deb/dists/stable/Release"
sign --armor --detach-sign --output "$out/deb/dists/stable/Release.gpg" "$out/deb/dists/stable/Release"

cat > "$out/index.html" <<EOF
<!doctype html>
<html lang="en">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>ContainerUI packages</title>
<style>body{font-family:system-ui,sans-serif;max-width:52rem;margin:2rem auto;padding:0 1rem;line-height:1.5}pre{background:#f4f4f4;padding:.75rem;overflow-x:auto}</style>
<h1>ContainerUI packages</h1>
<p>Signed with <a href="containerui.asc">this key</a>. The $keep newest releases are kept.</p>
<h2>Ubuntu and Debian</h2>
<pre>sudo install -d -m 0755 /etc/apt/keyrings
curl -fsSL $url/containerui.gpg | sudo tee /etc/apt/keyrings/containerui.gpg &gt;/dev/null
echo "deb [signed-by=/etc/apt/keyrings/containerui.gpg] $url/deb stable main" | sudo tee /etc/apt/sources.list.d/containerui.list
sudo apt update &amp;&amp; sudo apt install containerui</pre>
<h2>openSUSE and SLES</h2>
<pre>sudo zypper addrepo $url/rpm/containerui.repo
sudo zypper refresh &amp;&amp; sudo zypper install containerui</pre>
<h2>Fedora, RHEL and Rocky Linux</h2>
<pre>sudo dnf config-manager addrepo --from-repofile=$url/rpm/containerui.repo
sudo dnf install containerui</pre>
<p>Source: <a href="https://github.com/$repo">github.com/$repo</a></p>
</html>
EOF

echo "built $out: ${#rpms[@]} rpm and ${#debs[@]} deb packages from $(echo $tags | wc -w) releases"
