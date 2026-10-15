# ContainerUI

A web UI for managing containers, written in Go. The UI talks to a
backend-neutral `Runtime` interface, so different container runtimes can be
plugged in. It drives the **WSL container CLI (`wslc`)**, **docker** or
**podman**, picked automatically at startup, and runs on Windows and Linux.

Features:

- **Dashboard**: running containers, images, volumes and networks (refreshes every 5 s)
- **Containers**: create (image, name, command, ports, env, volumes, network, pull policy), start, stop, restart, delete / force delete, inspect
  - A collapsible **Advanced options** section on the create form adds
    `--tty` and `--interactive` (on by default), `--rm`, `--no-healthcheck`,
    `--cpus`, `--memory`, `--hostname`, `--user`, `--dns`, `--dns-search`,
    `--dns-option` and `--ulimit`. Empty fields are not passed; every value is
    validated server-side before it reaches the runtime.
- **Terminal**: the detail page has a *Connect* button next to the log
  buttons that opens an interactive shell (`<cli> container exec -it`) in a
  new tab. It checks first that the container runs and has a shell (bash,
  else sh): the button is blue when a terminal can be opened and grey and
  disabled otherwise, with the reason as its tooltip. Up to 8 terminals can
  be open at once; *Disconnect*, closing the tab or signing out ends the shell.
- **Logs**: the detail page offers *Download logs* (the container's complete
  output, stdout and stderr, as `<container-name>_<YYYYMMDD-HHMMSS>.log`)
  and, for a running container, *Watch logs*, which opens a new tab that
  shows the last 100 lines and then follows the output live as plain text
  under a fixed header with the container's details.
- **Images**: pull (with live progress), tag, delete / force delete
- **Volumes**: list (with the containers using each volume), create (name,
  driver — `guest`/`vhd` for wslc, `local` for docker/podman — driver options, labels), delete / force delete, prune
  (optionally including unused named volumes, `--all`) and a detail page with
  the inspect output. Force delete passes `volume remove --force`, which
  only ignores a missing volume: a volume in use by a container is never
  removed — delete the container first.
- **Networks**: list (with the containers attached to each network),
  create (name, driver, and optionally subnet, gateway, IP range,
  `--internal`, `--ipv6` on docker/podman, driver options, labels), delete,
  prune, and a detail page. The detail page shows subnets, labels, options
  and the attached containers with their addresses and aliases, plus the
  inspect output. From there you can **connect** a container (optional
  aliases and fixed IPv4) or **disconnect** one. The runtime's own networks
  (`bridge`, `host`, `none`, `podman`) are marked *built-in* and cannot be
  deleted. A network with connected containers is never removed.
- **Compose** (docker and podman only, see [Compose](#compose)): lists every
  compose project on the runtime, also those started outside ContainerUI,
  with its services, containers, volumes and networks. You can create a
  project by pasting, opening or dropping a compose file (checked live as you
  type), start, stop, restart, pull, take it down, and edit it. When the
  original file is not available, it is rebuilt from the containers.
- Login with system accounts: PAM on Linux, Windows accounts on Windows, for
  users who may use the container runtime's socket or pipe; plus optional
  accounts from the config file (Windows); served over HTTP or HTTPS
- Installed from an **rpm**, **deb** or **msi** package as a service (systemd as root,
  or a Windows service as LocalSystem), see [Install](#install)

## Requirements

- **Windows**: Windows 11 with **WSL 2.9.3 or newer** (`wsl --update`), which
  provides `wslc.exe`; or docker / podman
- **Linux** (amd64, arm64): docker or podman, and PAM (`libpam.so.0`,
  present on every standard distribution)
- Go 1.27+ to build

## Install

Releases are packages, built by the [release workflow](.github/workflows/release.yml)
for amd64 and arm64. An update is the package of the newer release,
installed the same way; the configuration is kept.

### Package repositories (Linux)

The Linux packages are also published as signed repositories on
[GitHub Pages](https://svenbeisiegel.github.io/containerui/), which keep the
five newest releases, so updates come with the system's own updates.

Ubuntu and Debian (apt):

```sh
sudo install -d -m 0755 /etc/apt/keyrings
curl -fsSL https://svenbeisiegel.github.io/containerui/containerui.gpg | sudo tee /etc/apt/keyrings/containerui.gpg >/dev/null
echo "deb [signed-by=/etc/apt/keyrings/containerui.gpg] https://svenbeisiegel.github.io/containerui/deb stable main" | sudo tee /etc/apt/sources.list.d/containerui.list
sudo apt update && sudo apt install containerui
```

openSUSE and SLES (zypper):

```sh
sudo zypper addrepo https://svenbeisiegel.github.io/containerui/rpm/containerui.repo
sudo zypper refresh && sudo zypper install containerui
```

Fedora, RHEL and Rocky Linux (dnf; `dnf config-manager --add-repo` on dnf 4):

```sh
sudo dnf config-manager addrepo --from-repofile=https://svenbeisiegel.github.io/containerui/rpm/containerui.repo
sudo dnf install containerui
```

### Linux (rpm)

```sh
sudo dnf install ./containerui-<version>-1.x86_64.rpm
```

| What | Where |
|---|---|
| Binary | `/usr/bin/containerui` |
| Service | `containerui.service` (`/usr/lib/systemd/system`), runs as **root**, enabled and started on install, restarted on upgrade |
| Configuration | `/etc/containerui/config.yaml` (`%config(noreplace)`: an upgrade keeps your file and puts the new default beside it as `.rpmnew`); the self-signed certificate is generated next to it |
| Compose projects | `/var/lib/containerui/compose` |
| Log | the journal: `journalctl -u containerui` |

The service listens on `0.0.0.0:8443`, so other machines can reach it; set
`server.listen` to `127.0.0.1:8443` and `systemctl restart containerui` to
allow local access only. Users sign in with their system account if they may use the
docker or podman socket, see [Linux sign-in](#linux-sign-in). `dnf remove
containerui` stops and removes the service and keeps the configuration
(changed files as `.rpmsave`).

### Linux (deb)

```sh
sudo apt install ./containerui_<version>-1_amd64.deb
```

The deb installs the same files and service as the rpm. The configuration is
a conffile: an upgrade keeps your changes (dpkg asks if the default changed
too), `apt remove containerui` keeps it and `apt purge containerui` deletes
it.

### Windows (msi)

```powershell
msiexec /i containerui-<version>-x64.msi              # with the setup dialogs
msiexec /i containerui-<version>-x64.msi /qn FIREWALL=0  # quietly, firewall left closed
```

| What | Where |
|---|---|
| Binary | `C:\Program Files\ContainerUI\containerui.exe` |
| Service | `ContainerUI`, runs as **LocalSystem**, starts with Windows, restarted after a crash |
| Configuration | `C:\ProgramData\ContainerUI\config.yaml` (SYSTEM and Administrators only; never overwritten by an update, kept by an uninstall) |
| Compose projects | `C:\ProgramData\ContainerUI\compose` |
| Log | `C:\ProgramData\ContainerUI\logs\containerui.log` (rotated by size) |

The msi allows `containerui.exe` through the Windows firewall, so other
machines can reach the service on `0.0.0.0:8443`. With `FIREWALL=0` nothing is
opened and only the local machine can reach it.
Users sign in with their Windows account, see [Windows sign-in](#windows-sign-in).

The service sees the **Docker** engine (Docker Desktop or Docker Engine,
through `\\.\pipe\docker_engine`). wslc sessions and podman machines belong
to the user who started them, so a LocalSystem service does not see them; in
service mode `auto` skips wslc. For wslc, run ContainerUI as that user
instead (see [wslc sessions are per user](#important-wslc-sessions-are-per-user)).

## Build

```powershell
.\build.ps1     # bin\containerui.exe and bin\containerui-linux-amd64
```

The Linux binary is cross-compiled without cgo (`CGO_ENABLED=0`): PAM is
loaded at runtime with [purego](https://github.com/ebitengine/purego), so no
C toolchain is needed. Manually:

```powershell
$env:GOOS='linux'; $env:GOARCH='amd64'; $env:CGO_ENABLED='0'; go build -o bin/containerui-linux-amd64 ./cmd/containerui
```

`build.ps1` stamps the version from [VERSION](VERSION) plus a timestamp
(`containerui version`). The packages are built on Linux with the
[Makefile](Makefile), the msi then on Windows with the WiX Toolset 5:

```sh
make packages VERSION=1.0.0   # dist/*.rpm, dist/*.deb (nfpm) and build/pkg/* (msi inputs)
```

```powershell
dotnet tool install --global wix --version 5.0.2
wix extension add --global WixToolset.Util.wixext/5.0.2
wix extension add --global WixToolset.Firewall.wixext/5.0.2
wix extension add --global WixToolset.UI.wixext/5.0.2
wix build packaging/windows/containerui.wxs -arch x64 -ext WixToolset.Util.wixext -ext WixToolset.Firewall.wixext -ext WixToolset.UI.wixext `
  -d Version=1.0.0 -d Bin=build\pkg\containerui_windows_amd64.exe -d Config=build\pkg\config.windows.yaml -o dist\containerui-1.0.0-x64.msi
```

A release is made by running the **Release** workflow with a version: it
commits `VERSION`, tests, builds the rpm, deb and msi files for amd64 and
arm64, publishes them with checksums as a GitHub release, and rebuilds the
package repositories on GitHub Pages from the five newest releases. It needs
the `PACKAGE_SIGNING_KEY` secret (an armored PGP private key, `-----BEGIN PGP
PRIVATE KEY BLOCK-----`), `PACKAGE_SIGNING_PASSPHRASE` if the key has one,
and Pages set to deploy from GitHub Actions. The rpms are signed with the
key, and so are the repositories' metadata. The **Test** workflow installs,
upgrades and removes the rpm in Rocky Linux 9, the deb in Ubuntu 24.04 and
the msi on Windows on every push.

## Quick start

```powershell
.\bin\containerui.exe
```

No config file is needed. Without `-config`, ContainerUI uses `config.yaml`
next to the executable and, if it does not exist, writes the default
configuration there (every setting commented). Missing settings always use
the built-in defaults.

- **Windows**: users sign in with their Windows account, see
  [Windows sign-in](#windows-sign-in). With `server.system_accounts: false`
  and no users in the config, ContainerUI asks on the console for a username
  and password on the first start and adds the account (bcrypt hash) to
  `config.yaml`.
- **Linux**: users sign in with their system account, see
  [Linux sign-in](#linux-sign-in).

Then open <https://localhost:8443/>. With `self_signed: true` the browser
warns about the certificate on first visit. Generated certificates are stored
next to the config file.

To try the UI without a container runtime, use `-driver fake` (in-memory
demo data).

## Configuration

The default file is [internal/config/default.yaml](internal/config/default.yaml)
(embedded in the binary). Settings:

| Key | Default | Meaning |
|---|---|---|
| `server.listen` | `0.0.0.0:8443` | Listen address |
| `server.tls.enabled` | `true` | Serve HTTPS |
| `server.tls.self_signed` | `true` (unless `cert_file`/`key_file` are set) | Generate and reuse a self-signed certificate |
| `server.tls.cert_file`/`key_file` | — | Your own certificate (PEM) |
| `server.session_idle_timeout` | `30m` | Log out after inactivity |
| `server.session_absolute_timeout` | `12h` | Maximum session lifetime |
| `server.pam_service` | `login` | Linux: PAM service for sign-in (`/etc/pam.d/<name>`) |
| `server.system_accounts` | `true` | Windows: Windows accounts may sign in, see [Windows sign-in](#windows-sign-in) |
| `runtime.driver` | `auto` | `auto`, `wslc`, `docker`, `podman` or `fake` |
| `runtime.path` | — | Path to the CLI (empty = `wslc.exe` / `docker` / `podman` on `PATH`) |
| `runtime.session` | — | wslc session to use (empty = default; wslc only) |
| `runtime.command_timeout` | `30s` | Timeout for regular commands |
| `runtime.pull_timeout` | `30m` | Timeout for pulls and container create |
| `runtime.compose_path` | — | A standalone compose binary (`docker-compose`, `podman-compose`); empty = detect, see [Compose](#compose) |
| `runtime.compose_timeout` | `30m` | Timeout for compose up/down/pull/start/stop |
| `runtime.compose_dir` | `compose` | Folder for the compose files deployed through the UI (relative to the config file) |
| `users[]` | — | Windows: additional accounts, `username` plus `password_hash` (bcrypt) or `password` (plaintext, warns). Ignored on Linux |

Configs using the earlier `runtime.wslc:` block (`path`, `session`,
timeouts) still load; they select the wslc driver.

## Container runtimes

With `driver: auto` ContainerUI uses the first installed CLI that answers a
version call: on Windows `wslc`, then `docker`, then `podman`; on Linux
`docker`, then `podman`. If none answers, it takes the first one installed
and the UI shows errors until it is reachable. The startup log names the
selected driver and binary; `-driver` overrides the choice.

All three accept the same commands and flags. The driver handles where they
differ:

| | wslc | docker | podman |
|---|---|---|---|
| Version check | `wslc version` | `docker version` (asks the daemon) | `podman version` |
| List output | NDJSON | NDJSON (same fields) | JSON arrays with podman's own fields |
| Errors | stable `WSLC_E_*` codes in localized text | English messages | English messages |
| Volume drivers | `guest`, `vhd` | `local` | `local` |
| Volume prune | anonymous only, or all with `--all` | same | always all unused volumes (no `--all`) |
| Network drivers | `bridge` | `bridge`, `macvlan`, `ipvlan`, `overlay` | `bridge`, `macvlan`, `ipvlan` |
| IPv6 networks | — | `--ipv6` | `--ipv6` |
| Container alias on connect | `--network-alias` | `--alias` | `--alias` |
| `--session` | yes | — | — |

## Compose

The Compose section uses the runtime's **native compose tool**. ContainerUI
does not interpret compose files itself. At startup the driver tries, in
order:

- docker: `docker compose` (plugin), then `docker-compose` on `PATH`
- podman: `podman compose`, then `podman-compose` on `PATH`

The first one whose `version` call succeeds is used. Set
`runtime.compose_path` to use a particular binary instead. Docker Compose v1
is not supported. **wslc has no compose**, so with wslc, or when no tool is
found, the Compose menu and its pages are hidden.

- **Projects** are found by the labels compose puts on everything it creates
  (`com.docker.compose.project`, `.service`, `.container-number`, …). The list
  therefore also shows projects deployed from the command line or by other
  tools. Containers of a project carry a project tag in the container list.
- **Create**: paste a compose file, open it with *Open file…* or drop it onto
  the editor. Every change is checked on the server:
  - YAML syntax, with the line number;
  - top-level keys;
  - each service has `image` or `build`;
  - lists and mappings have the right shape;
  - references to undefined services, networks and volumes.

  *Check* also runs the compose tool's own validation (`config`).
  The project name comes from the form or the file's `name:`.
- **Stored files**: a deployed file is saved as
  `<compose_dir>/<project>/compose.yaml` and deployed with
  `compose -p <project> -f <that file> up --detach --remove-orphans`. That
  folder is the project directory, so relative paths (bind mounts,
  `env_file`, `.env`) resolve inside it; put such files there. Operations run
  as background jobs with their output shown live. Only one runs per project
  at a time.
- **Edit / update**: the editor is filled from the first of these that
  exists:
  1. the stored file;
  2. the file named in the `config_files` label, if it is readable on this
     host;
  3. a file **rebuilt from the containers**.

  Applying stores the file and runs `up` again, which recreates only
  services whose configuration changed and removes services that were
  dropped.
- **Rebuilding** uses container and image inspect data:
  - image, command and entrypoint (only where they differ from the image);
  - environment (minus the image's own);
  - ports, volumes (named volumes mapped back to their compose names, others
    marked `external`) and networks;
  - `depends_on`, restart policy, healthcheck, labels, resources and replicas.

  What the runtime does not record is lost: build contexts, `env_file`,
  profiles, variables, comments, secrets and configs. Review the result
  before applying it.
- **Actions** without a stored file address the project by name
  (`compose -p <project> start|stop|restart|down`). Pull needs the file.
  *Down* keeps volumes unless *Also remove the project's volumes* is
  checked. A stored but undeployed project can be deployed again or its
  file deleted.

## Linux sign-in

On Linux there is no user list: people sign in with their **system account**.
A sign-in succeeds when

1. PAM accepts the password (service `server.pam_service`, default `login`,
   including account checks such as expiry), **and**
2. the account may use the container runtime, i.e. it could connect to the
   runtime's socket itself: it is root, owns the socket, or is in the
   socket's group with read/write permission (for docker usually the
   `docker` group; the socket is `$DOCKER_HOST` or `/var/run/docker.sock`,
   for rootful podman `/run/podman/podman.sock`). For a runtime without a
   socket (rootless podman) only root and the account ContainerUI runs as
   may sign in.

Run ContainerUI as **root** (the rpm's systemd service does): PAM can only
check other users' passwords with root rights. Users listed in the config are
ignored (with a warning). Every signed-in user works with the runtime as
ContainerUI does (as root), which is what access to the docker socket grants
anyway.

## Windows sign-in

On Windows people sign in with their **Windows account** (`user`,
`DOMAIN\user` or `user@domain`) unless `server.system_accounts` is `false`;
accounts listed under `users` in the config sign in as well. A Windows
sign-in succeeds when

1. Windows accepts the password (a network logon, `LogonUser`), **and**
2. the account may use the container runtime: the security descriptor of its
   named pipe grants it read/write (Docker: `\\.\pipe\docker_engine` or the
   `npipe://` in `DOCKER_HOST`, open to `docker-users` and Administrators).
   If the pipe cannot be read (runtime stopped), membership in Administrators
   or `docker-users` decides. Administrators whose network logon UAC filters
   are checked with their full token. For wslc, which has no pipe, only
   Administrators and the account ContainerUI runs as may sign in.

## Important: wslc sessions are per user

wslc keeps one session per Windows user and per elevation level. ContainerUI
only sees the containers of the account it runs under. Run it as the same,
**non-elevated** user who uses `wslc` on the command line, not as a
LocalSystem service and not from an elevated prompt (unless that is where
your containers live). The msi's service therefore uses Docker; started as a
service, `driver: auto` does not consider wslc.

### Cold starts

wslc stops its session after roughly 25–30 seconds without commands, and the
next command then takes 2–3 seconds while it starts again. ContainerUI hides
this where it can:

- At startup it loads all dashboard data once ("warming up container
  runtime" … "container runtime ready") before it starts listening.
- Opening the sign-in page starts a background warm-up, and the page keeps
  the session alive (one cheap list call every 15 s, for at most 10 minutes)
  while the user types. Warm-ups run at most once every 10 seconds.
- While a dashboard or container list is open, its 5-second refresh keeps the
  session warm anyway.

The VM is never kept running when nobody has the UI open.

## Appearance

The UI follows the Mitel brand palette (Dark Blue `#15325f`, Mid Blue
`#0073d0`, Light Blue `#00a1e0`, with Yellow/Purple accents) and has a light
and a dark theme. It starts in the browser's/system's preference; the toggle
in the top bar (or top-right corner of the sign-in page) switches theme and
remembers the choice in the browser (`localStorage`).

Headings use **Museo Sans** if it is installed on the viewer's machine; the
font is licensed and therefore not bundled. The fallback is Arial, the brand's
official alternative.

**Favicon:** none is shipped. Put an official Mitel icon from the brand portal
into `internal/web/static/` as `favicon.svg`, `favicon.png` or `favicon.ico`
(first match wins) and rebuild; it is embedded and linked automatically.

## Architecture

```
cmd/containerui        entry point: serve, hash-password, first-start user setup
internal/config        defaults, embedded default.yaml, loading, validation, AddUser
internal/runtime       Runtime interface, domain types, validation, driver registry, Composer
  cli/                 driver for wslc, docker and podman (--format json), auto-detection
  fake/                in-memory driver for tests and demos
internal/auth          bcrypt users, Linux PAM sign-in, sessions, CSRF, login throttling
internal/compose       compose file checks, stored files, grouping by labels, rebuilding a file
internal/jobs          background jobs (image pulls, compose operations) with progress
internal/web           HTTP server, handlers, html/template + htmx (embedded)
internal/tlsutil       self-signed certificate generation
```

### Why the wslc CLI instead of `wslcsdk.dll`

The WSL container C SDK (`wslcsdk.dll`, NuGet `Microsoft.WSL.Containers`)
currently has three gaps:

- no functions to list containers, volumes or networks
- no restart
- no way to attach to the CLI's default session; SDK sessions are separate,
  see [microsoft/WSL#40966](https://github.com/microsoft/WSL/issues/40966)
  and [microsoft/WSL#40990](https://github.com/microsoft/WSL/issues/40990)

The CLI covers all features and emits JSON (`--format json`). Its
human-readable messages are localized, so the driver only parses JSON and the
stable `WSLC_E_*` / `ERROR_*` error codes. Once the SDK closes these gaps, an
SDK driver can be added behind the same interface.

### Terminal

The browser runs [xterm.js](https://xtermjs.org/) 5.5 with the fit addon
(MIT, vendored in `internal/web/static/vendor/xterm/`). It talks to
`/containers/{id}/terminal/ws` over a WebSocket: binary messages carry raw
terminal bytes, text messages carry JSON control messages (`resize` from the
browser, `exit` / `error` from the server).

The CLIs only allocate a TTY when their own stdio is a terminal, so the
server starts `<cli> container exec --interactive --tty` on a pseudo terminal
(ConPTY on Windows, a Unix PTY on Linux, via `github.com/aymanbagabas/go-pty`)
and pipes it to the socket. The CLI does not stop the process it started
when it exits, so every terminal
gets a random `CONTAINERUI_TERMINAL` environment tag, and closing a terminal
sends SIGHUP (then SIGKILL) to all container processes carrying that tag.
This needs `tr` and `grep` in the container, which every image with a shell
has in practice.

### Logs

Both log features run `<cli> container logs` with stdout and stderr merged in
arrival order. The download (`/containers/{id}/logs/download`) streams the
output straight into the response. The live view
(`/containers/{id}/logs/watch`) is a small standalone page whose script reads
`/containers/{id}/logs/stream`, a Server-Sent Events stream of
`<cli> container logs --follow --tail 100` (`?tail=N` overrides, 0 = all).
Each event carries one or more lines as `data:` fields; a final `end` event
says why the stream stopped (container stopped, error, session ended). The
page does not use the browser's automatic SSE reconnect, which would replay
the initial lines; it offers a *Reconnect* button instead and keeps at most
20,000 lines.

### Adding a driver

Implement `runtime.Runtime` in a new package under `internal/runtime/`,
register it in `init()` with `runtime.Register("name", factory)`, and import
it in `cmd/containerui/main.go`. Report unsupported features through
`Capabilities()` so the UI hides them.

Another docker-compatible CLI only needs a new `Flavor` in
`internal/runtime/cli/flavor.go` (binary, version command, error
classification, volume drivers) and an entry in `Flavors`.

## Security notes

- Passwords are stored as bcrypt hashes. Unknown users still go through a
  bcrypt compare, so usernames can't be probed by timing. Repeated failed
  logins per client IP and username are locked out with exponential backoff.
- Sessions are server-side; the cookie is `HttpOnly`, `SameSite=Strict`, and
  `Secure` when TLS is on.
- Every state-changing request needs a per-session CSRF token and must come
  from the same origin.
- A strict Content-Security-Policy is set; all assets, including htmx 2.0.4
  and xterm.js, are embedded in the binary. The terminal page alone
  also allows inline *styles* (`style-src 'unsafe-inline'`), which xterm.js
  needs for its renderer; scripts stay restricted to `'self'`.
- The terminal WebSocket requires a signed-in session, the session's CSRF
  token and a same-origin `Origin` header. The server repeats the shell check
  itself; nothing the browser sends selects the command that runs. Open
  terminals close when the session ends (checked every 30 s; typing in a
  terminal counts as activity, an idle one does not keep the session alive).
  Terminal open/close is logged with user and container, never the content.
- The log endpoints are read-only GETs behind the sign-in. A live log stream
  closes when the session ends (checked every 15 s; watching does not keep
  the session alive), at most 16 streams can be open at once, and the page
  inserts log output as text, never as HTML. Downloads and watch open/close
  are logged with user and container.
- On Linux, passwords are checked by PAM and access additionally requires
  permission on the runtime's socket, so only people who could use docker /
  podman directly can use the UI.
- All user input is validated before it reaches the CLI and is passed as
  separate arguments (no shell), so values can't be read as flags.
- Network create, delete, prune, connect and disconnect are validated
  before they reach the CLI: names; subnets, gateways and IP ranges (which
  must lie inside the subnet); aliases; IPv4 addresses. Each operation is
  logged with the user.
- A compose file can do everything compose can, including bind-mounting any
  host path and `privileged: true`. Deploying one therefore needs the same
  trust as using the runtime directly. Every compose operation is logged
  with user and project. Project names are validated, so stored files
  cannot leave `compose_dir`.

## Development

```powershell
go test ./...                                              # unit + web tests
go test -tags integration ./internal/runtime/cli/...       # against real wslc (pulls alpine)
go run ./cmd/containerui -config config.yaml -driver fake -debug
```

## License

MIT, see [LICENSE](LICENSE).
