# Em dash audit for the release gate (docs/protocol/VERIFICATION_PROTOCOL.md, section 6).
# Fails if any tracked Markdown file outside the grandfathered verbatim set
# contains U+2014 (em dash). The grandfathered files are historical documents
# kept as written (docs/protocol/README.md); no new file may join the list.
$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
Push-Location $repoRoot
try {
    $grandfathered = @(
        'docs/protocol/PHASE0_BUILD_BRIEF.md',
        'docs/protocol/PHASE3_BUILD_BRIEF.md',
        'docs/protocol/SABOTEUR_INJECTION.md',
        'docs/design/01-schema.design.md',
        'docs/design/02-recorder.verify.md',
        'docs/design/03-harness.design.md',
        'docs/design/04-fixture.design.md',
        'docs/design/saboteur/06-saboteur.architecture.md',
        'docs/design/saboteur/07-saboteur.i2-conflict.md'
    )
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
