# Builds ContainerUI for Windows and Linux (amd64) into bin\, stamped with a
# dev version (VERSION plus a timestamp). The Linux build needs no C
# toolchain (CGO_ENABLED=0; PAM is loaded at runtime). Release packages (rpm,
# msi) are built by the Makefile and the release workflow.
$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot

$version = "$((Get-Content VERSION -Raw).Trim())-dev-$(Get-Date -Format 'yyyyMMdd-HHmmss')"
$ldflags = "-s -w -X main.version=$version"

$targets = @(
    @{ GOOS = 'windows'; GOARCH = 'amd64'; Out = 'bin/containerui.exe' },
    @{ GOOS = 'linux';   GOARCH = 'amd64'; Out = 'bin/containerui-linux-amd64' }
)

$saved = @{ GOOS = $env:GOOS; GOARCH = $env:GOARCH; CGO_ENABLED = $env:CGO_ENABLED }
try {
    foreach ($t in $targets) {
        $env:GOOS = $t.GOOS
        $env:GOARCH = $t.GOARCH
        $env:CGO_ENABLED = '0'
        Write-Host "building $($t.Out) ($version)"
        go build -trimpath -ldflags $ldflags -o $t.Out ./cmd/containerui
        if ($LASTEXITCODE -ne 0) { throw "go build failed for $($t.GOOS)/$($t.GOARCH)" }
    }
} finally {
    $env:GOOS = $saved.GOOS
    $env:GOARCH = $saved.GOARCH
    $env:CGO_ENABLED = $saved.CGO_ENABLED
}
