# Run the race detector for both modules inside a Linux container.
#
# The race detector needs cgo and a C compiler. This host has CGO_ENABLED=0 and
# no gcc, and installing a C toolchain to run one test would be a poor trade
# when Docker Desktop's Linux VM is already a hard dependency of the compose
# backend. ThreadSanitizer is also far better exercised on Linux than on
# Windows. See DECISIONS.md D-023.
#
# Nothing is installed on the host.
#
# usage: pwsh scripts/test-race.ps1 [-Image golang:1.22] [-Verbose]

param(
    [string]$Image = "golang:1.22",
    [switch]$VerboseTests
)

$ErrorActionPreference = "Stop"
$repo = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path

Write-Host "race runner: $Image" -ForegroundColor Cyan
Write-Host "workspace  : $repo"

# The image pins the Go version declared in go.mod, not the newer toolchain that
# wrote the code. That is deliberate: it keeps the `go 1.22` declaration honest
# rather than aspirational.
$flags = if ($VerboseTests) { "-v" } else { "" }

# `sh -c`, never `sh -lc`. A login shell re-runs /etc/profile and drops Go from
# PATH, which surfaces as a baffling "go: not found".
$script = @"
set -e
go version
echo '===== ROOT MODULE ====='
CGO_ENABLED=1 go test -race $flags ./...
echo '===== FIXTURE MODULE ====='
cd testdata/kvfixture
CGO_ENABLED=1 go test -race $flags ./...
"@

docker run --rm `
    -v "${repo}:/workspace" `
    -w /workspace `
    -e CGO_ENABLED=1 `
    $Image `
    sh -c $script

if ($LASTEXITCODE -ne 0) {
    Write-Host "RACE DETECTED or tests failed (exit $LASTEXITCODE)" -ForegroundColor Red
    exit $LASTEXITCODE
}
Write-Host "race-clean in both modules" -ForegroundColor Green
