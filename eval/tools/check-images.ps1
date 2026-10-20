# Check that every world of a run ran on the pinned image (evaluation-suite rule).
#
# A world counts toward a phase's figures only if the image it actually ran is the image the arm
# pinned. The evidence is the world's own record: world.thesis `sut.images`, which the harness fills
# from `docker inspect` on the bound containers (the container's resolved image ID, not the compose
# text). The pin is the `@sha256:` digest of the target's compose `image:` reference, or -Pin.
#
#   - MATCH     every recorded image entry carries the pinned digest
#   - MISMATCH  some entry carries another digest (a mixed fleet, a re-pulled tag, a local build)
#   - MISSING   no world.thesis, or no `sut.images` in it (the harness's lookup is fail-soft)
#
# The comparison is strict. On a containerd image store the resolved ID is the manifest digest, so
# it equals the pin; on the classic store it is the config digest and every world will read
# MISMATCH. That is a refusal to vouch, not a verdict on the world, and the closing note says so
# when a mismatched entry asked for the pinned reference itself.
#
# Images of compose services that are not harness nodes (sidecars, shims) are not recorded by the
# harness and so are not checked here.
#
# Usage:
#   pwsh -File eval/tools/check-images.ps1 -Project targets/rqlite -Run r_2026_10_20_95ab [-Run ...] [-Pin ref@sha256:<hex>]
#
# Exit codes: 0 every world of every listed run is MATCH; 1 any world is MISMATCH or MISSING, or a
# run is not found; 2 bad arguments or no pin.

[CmdletBinding()]
param(
    [Parameter(Mandatory)] [string] $Project,
    [Parameter(Mandatory)] [string[]] $Run,
    [string] $Pin = ''
)

$ErrorActionPreference = 'Stop'

function Fail-Args([string] $msg) { [Console]::Error.WriteLine("check-images: $msg"); exit 2 }

if (-not (Test-Path $Project -PathType Container)) { Fail-Args "no project directory at $Project" }

if ($Pin -eq '') {
    $compose = Join-Path $Project 'docker-compose.yaml'
    if (-not (Test-Path $compose)) { Fail-Args "no -Pin and no $compose" }
    $refs = @(Select-String -LiteralPath $compose -Pattern '^\s*image:\s*"?([^"\s#]+)"?' |
        ForEach-Object { $_.Matches[0].Groups[1].Value } | Sort-Object -Unique)
    if ($refs.Count -ne 1) { Fail-Args ("{0} distinct image references in {1}; pass -Pin" -f $refs.Count, $compose) }
    $Pin = $refs[0]
}
if ($Pin -notmatch '@(sha256:[0-9a-f]{64})$') { Fail-Args "pin carries no sha256 digest: $Pin" }
$want = $Matches[1]
Write-Host "pin: $Pin"

$runsDir = Join-Path $Project '.prothesis/runs'
$bad = 0
$storeHints = 0
foreach ($r in $Run) {
    if ($r -notmatch '^r_\d{4}_\d{2}_\d{2}_[0-9a-f]{4,}$') { Fail-Args "not a run id: $r" }
    $dir = Join-Path $runsDir $r
    if (-not (Test-Path $dir -PathType Container)) { Write-Host "$r : NOT FOUND in $runsDir"; $bad++; continue }

    $worlds = @(Get-ChildItem $dir -Directory -Filter 'world-*' | Sort-Object Name)
    if ($worlds.Count -eq 0) { Write-Host "$r : no world directories"; $bad++; continue }
    foreach ($w in $worlds) {
        $file = Join-Path $w.FullName 'world.thesis'
        $images = @()
        if (Test-Path $file) {
            $doc = Get-Content -LiteralPath $file -Raw | ConvertFrom-Json
            if ($doc.sut -and $doc.sut.images) { $images = @($doc.sut.images) }
        }
        if ($images.Count -eq 0) { Write-Host "$r/$($w.Name) : MISSING (no sut.images recorded)"; $bad++; continue }

        $other = @($images | Where-Object { $_.digest -ne $want })
        if ($other.Count -eq 0) {
            Write-Host ("{0}/{1} : MATCH ({2} entr{3})" -f $r, $w.Name, $images.Count, $(if ($images.Count -eq 1) { 'y' } else { 'ies' }))
        } else {
            foreach ($o in $other) {
                Write-Host ("{0}/{1} : MISMATCH service {2} ran {3} ({4})" -f $r, $w.Name, $o.service, $o.digest, $o.image)
                if ("$($o.image)".EndsWith("@$want")) { $storeHints++ }
            }
            $bad++
        }
    }
}

if ($storeHints -gt 0) {
    Write-Host "note: $storeHints mismatched entr$(if ($storeHints -eq 1) { 'y' } else { 'ies' }) asked for the pinned reference itself but recorded another ID, as a classic image store does (the ID is the config digest); the check cannot vouch for those worlds."
}
exit ($(if ($bad -gt 0) { 1 } else { 0 }))
