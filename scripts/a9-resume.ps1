# Resume the A.9 benchmark's `random` arm from a given seed, appending to the CSV.
#
# DOT-SOURCE THIS, like a9-benchmark-inline.ps1, and for the same reason.
#
# Resuming is legitimate here only because the thing that stopped the first
# attempt was the fingerprint guard firing on a `_test.go` file: a file
# `go run ./cmd/thesis` never compiles. Every trial in this CSV, before and
# after the interruption, therefore ran the SAME program, and the fingerprint
# below is checked against the one those trials ran under so that claim is
# verified rather than asserted.
#
# It keeps both premise checks:
#   - every trial must produce a run id not already in the CSV
#   - the COMPILED source must not change mid-run

param(
    [string]$Repo        = (Split-Path -Parent $PSScriptRoot),
    [string]$Arm         = "random",
    [int]   $FromSeed    = 3,
    [int]   $ToSeed      = 10,
    [string]$Out         = "$env:TEMP\a9_benchmark.csv",
    [string]$LogDir      = "$env:TEMP\a9logs",
    [string]$Budget      = "8m",
    [int]   $Workers     = 4,
    [string]$Fingerprint = ""
)

$Config  = "testdata\kvfixture\prothesis.yaml"
$Fixture = Join-Path $Repo "testdata\kvfixture"

New-Item -ItemType Directory -Force -Path $LogDir | Out-Null
Set-Location $Repo

function SourceFingerprint {
    $files = Get-ChildItem -Path (Join-Path $Repo "cmd"), (Join-Path $Repo "internal"), (Join-Path $Repo "pkg") `
        -Recurse -File -Filter *.go |
        Where-Object { $_.Name -notlike "*_test.go" } | Sort-Object FullName
    $acc = [System.Text.StringBuilder]::new()
    foreach ($f in $files) {
        [void]$acc.Append((Get-FileHash -Algorithm SHA256 -LiteralPath $f.FullName).Hash)
    }
    $sha   = [System.Security.Cryptography.SHA256]::Create()
    $bytes = [System.Text.Encoding]::UTF8.GetBytes($acc.ToString())
    return ($sha.ComputeHash($bytes) | ForEach-Object { $_.ToString("x2") }) -join ""
}

$fp = SourceFingerprint
Write-Host "compiled-source fingerprint: $fp"
if ($Fingerprint -ne "" -and $Fingerprint -ne $fp) {
    throw "the compiled source differs from the trials already in $Out ($Fingerprint -> $fp). Resuming would mix two programs; re-run the whole benchmark instead."
}

# Every run id already recorded, so a resumed trial cannot re-report one.
$seen = @{}
foreach ($row in (Import-Csv $Out)) { $seen[$row.run_id] = "$($row.arm) seed $($row.seed)" }
Write-Host "already recorded: $($seen.Count) trial(s)"

for ($seed = $FromSeed; $seed -le $ToSeed; $seed++) {
    $log = Join-Path $LogDir "$($Arm)_$($seed).log"
    $sw = [Diagnostics.Stopwatch]::StartNew()
    go run ./cmd/thesis search --config $Config --profile search --strategy $Arm `
        --workers $Workers --budget $Budget --seed $seed `
        --worker-env "prefix=KV_PREFIX,kv-n1=KV_PORT_N1,kv-n2=KV_PORT_N2,kv-n3=KV_PORT_N3" `
        > $log 2>&1
    $sw.Stop()

    $runDir = Get-ChildItem "$Fixture\.prothesis\runs" -Directory |
        Sort-Object LastWriteTime | Select-Object -Last 1
    $runId = $runDir.Name

    if ($seen.ContainsKey($runId)) {
        throw "[$Arm seed $seed] run id $runId already recorded for $($seen[$runId]); this trial produced no run of its own. See $log."
    }
    $seen[$runId] = "$Arm seed $seed"

    $now = SourceFingerprint
    if ($now -ne $fp) { throw "[$Arm seed $seed] the compiled source changed mid-run ($fp -> $now); discard." }
    if (-not (Test-Path "$($runDir.FullName)\verdict.json")) { throw "[$Arm seed $seed] run $runId wrote no verdict.json" }

    $worlds = 0; $ord = 0; $nf = 0; $secs = ""
    if (Test-Path "$($runDir.FullName)\search.json") {
        $rec = Get-Content "$($runDir.FullName)\search.json" -Raw | ConvertFrom-Json
        $worlds = $rec.worlds_run; $ord = $rec.first_violation_ordinal
        $nf = $rec.first_violation_fault_count
        if ($null -ne $rec.first_violation_seconds) { $secs = $rec.first_violation_seconds }
    }
    $v = Get-Content "$($runDir.FullName)\verdict.json" -Raw | ConvertFrom-Json
    $found = 0
    if ($v.violations | Where-Object { $_.oracle -eq "linearizable.kv" }) { $found = 1 }

    "{0},{1},{2},{3},{4:N1},{5},{6},{7},{8},{9}" -f `
        $Arm, $seed, $v.verdict, $worlds, $sw.Elapsed.TotalSeconds, $found, $ord, $nf, $secs, $runId |
        Out-File -FilePath $Out -Encoding utf8 -Append
    Write-Host ("[{0} seed {1}] {2} worlds={3} {4:N0}s consistency={5} run={6}" -f `
        $Arm, $seed, $v.verdict, $worlds, $sw.Elapsed.TotalSeconds, $found, $runId)
}
Write-Host "appended to $Out"
