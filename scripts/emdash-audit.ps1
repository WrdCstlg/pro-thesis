# Em dash audit for the release gate (section 6 of the verification protocol,
# which is private since D-105).
# Fails if any tracked Markdown file outside the grandfathered verbatim set
# contains U+2014 (em dash). The grandfathered set held historical protocol and
# design documents kept as written; they moved to the private companion
# repository (D-105), so the set is empty. No file may join it.
$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
Push-Location $repoRoot
try {
    $grandfathered = @()
    $violations = @()
    $files = git ls-files -- '*.md'
    foreach ($f in $files) {
        $norm = $f -replace '\\', '/'
        if ($grandfathered -contains $norm) { continue }
        $text = [IO.File]::ReadAllText((Join-Path $repoRoot $norm))
        $count = ([regex]::Matches($text, [char]0x2014)).Count
        if ($count -gt 0) {
            $violations += "${norm}: $count em dash(es)"
        }
    }
    if ($violations.Count -gt 0) {
        Write-Host 'EM DASH AUDIT FAILED:'
        $violations | ForEach-Object { Write-Host "  $_" }
        exit 1
    }
    Write-Host "em dash audit OK ($($files.Count) markdown files scanned, $($grandfathered.Count) grandfathered)"
    exit 0
} finally {
    Pop-Location
}
