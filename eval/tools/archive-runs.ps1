# Archive completed evaluation-suite run directories (retention policy P0).
#
# Policy (recorded in the project's decision ledger):
#   - Only runs created by the evaluation suite are archived. The pre-existing fixture corpus is
#     never touched, because a committed census floor guards it (testdata/kvfixture/.prothesis/
#     CENSUS.json) and shrinking it would fail that check.
#   - A run is archived only after its phase is scored and cross-checked, and never if it is
#     cited as evidence (pass -Keep for those, or simply do not list them).
#   - Archiving never deletes. Each run directory is compressed into
#     <project>/.prothesis/archive/<run>.tar.zst, every file's SHA-256 is recorded next to it, the
#     archive is extracted to a temporary directory and compared file by file, and only then is
#     the original directory RENAMED into <project>/.prothesis/runs/.trash/<run>. Reclaiming
#     .trash is a separate, explicitly instructed sweep.
#
# Usage:
#   pwsh -File eval/tools/archive-runs.ps1 -Project testdata/kvfixture -Run r_2026_10_20_53a0 [-Run ...] [-WhatIf]
#
# Exit codes: 0 every listed run archived and verified (or already archived); 1 a verification
# failed and the original was left in place; 2 bad arguments.

[CmdletBinding()]
param(
    [Parameter(Mandatory)] [string] $Project,
    [Parameter(Mandatory)] [string[]] $Run,
    [switch] $WhatIf
)

$ErrorActionPreference = 'Stop'

function Get-FileHashes([string] $dir) {
    $root = (Resolve-Path $dir).Path.TrimEnd('\')
    $map = [ordered]@{}
    Get-ChildItem $root -Recurse -File -Force | Sort-Object FullName | ForEach-Object {
        $rel = $_.FullName.Substring($root.Length + 1).Replace('\', '/')
        $map[$rel] = (Get-FileHash $_.FullName -Algorithm SHA256).Hash.ToLower()
    }
    return $map
}

function Fail-Args([string] $msg) { [Console]::Error.WriteLine("archive-runs: $msg"); exit 2 }

if (-not (Test-Path $Project -PathType Container)) { Fail-Args "no project directory at $Project" }
$runsDir = Join-Path $Project '.prothesis/runs'
$archiveDir = Join-Path $Project '.prothesis/archive'
$trashDir = Join-Path $runsDir '.trash'

$failed = 0
foreach ($r in $Run) {
    if ($r -notmatch '^r_\d{4}_\d{2}_\d{2}_[0-9a-f]{4,}$') { Fail-Args "not a run id: $r" }
    $src = Join-Path $runsDir $r
    $arc = Join-Path $archiveDir "$r.tar.zst"
    $sums = Join-Path $archiveDir "$r.sha256.json"

    if (-not (Test-Path $src -PathType Container)) {
        if ((Test-Path $arc) -and (Test-Path $sums)) { Write-Host "$r : already archived"; continue }
        Write-Host "$r : NOT FOUND in $runsDir and not archived"; $failed++; continue
    }

    $before = Get-FileHashes $src
    Write-Host ("{0} : {1} files" -f $r, $before.Count)
    if ($WhatIf) { Write-Host "  (WhatIf) would archive to $arc and move the directory to $trashDir"; continue }

    New-Item -ItemType Directory -Force $archiveDir | Out-Null
    & tar --zstd -cf $arc -C $runsDir $r
    if ($LASTEXITCODE -ne 0) { Write-Host "  tar failed (exit $LASTEXITCODE); original left in place"; $failed++; continue }

    $check = Join-Path ([IO.Path]::GetTempPath()) ("archive-verify-" + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory $check | Out-Null
    & tar --zstd -xf $arc -C $check
    $after = if ($LASTEXITCODE -eq 0) { Get-FileHashes (Join-Path $check $r) } else { @{} }

    $mismatch = @()
    foreach ($k in $before.Keys) { if ($after[$k] -ne $before[$k]) { $mismatch += $k } }
    foreach ($k in $after.Keys) { if (-not $before.Contains($k)) { $mismatch += $k } }
    [IO.Directory]::Delete($check, $true)

    if ($mismatch.Count -gt 0) {
        Write-Host ("  VERIFY FAILED: {0} file(s) differ, e.g. {1}; original left in place" -f $mismatch.Count, $mismatch[0])
        $failed++; continue
    }

    [ordered]@{
        run         = $r
        project     = $Project
        archive     = (Split-Path $arc -Leaf)
        archive_sha = (Get-FileHash $arc -Algorithm SHA256).Hash.ToLower()
        files       = $before
    } | ConvertTo-Json -Depth 4 | Set-Content -Encoding utf8 $sums

    New-Item -ItemType Directory -Force $trashDir | Out-Null
    Move-Item -LiteralPath $src -Destination (Join-Path $trashDir $r)
    Write-Host ("  archived and verified ({0} files); original moved to .trash" -f $before.Count)
}

exit ($(if ($failed -gt 0) { 1 } else { 0 }))
