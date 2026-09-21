# A.9 item 4: the pre-registered comparison, as executed.
#
# DOT-SOURCE THIS. Do not run it with `pwsh -File`: a nested pwsh whose stdout
# is redirected cannot launch a native binary on this host.
#
# The design is frozen in testdata/kvfixture/.prothesis/PREREGISTRATION.md and
# was written BEFORE any trial ran. This file only executes it.
#
# ---------------------------------------------------------------------------
# TWO THINGS ABOUT THIS FILE THAT ARE NOT INCIDENTAL
#
# 1. IT LAUNCHES VIA `go run`, NOT A BUILT BINARY. This machine's Application
#    Control / Device Guard policy blocks a freshly built .exe: from %TEMP%,
#    from the repo, via Start-Process, via cmd.exe and via PowerShell's `&`
#    alike. PowerShell reports the block as "StandardOutputEncoding is only
#    supported when standard output is redirected", which looks like a
#    redirection bug and is not. `go run` spawns through the Go toolchain, which
#    the policy permits (it is how the test binaries run).
#
# 2. IT ASSERTS THAT EACH TRIAL PRODUCED A RUN OF ITS OWN. The first version did
#    not, and it produced a perfect result without executing anything: the
#    launch failed silently, the script read `.prothesis/runs`, found the
#    PREVIOUS run's directory and recorded its outcome as this trial's. All
#    twenty rows carried the same run_id and the same 107.5-second
#    time-to-violation, and the arms tied at 10/10: a benchmark that could not
#    fail, reporting on code it never ran. The run-id check below is what turns
#    these numbers into evidence.
# ---------------------------------------------------------------------------

$Repo    = Split-Path -Parent $PSScriptRoot
$Config  = "testdata\kvfixture\prothesis.yaml"
$Fixture = Join-Path $Repo "testdata\kvfixture"
$Out     = Join-Path $env:TEMP "a9_benchmark.csv"
$LogDir  = Join-Path $env:TEMP "a9logs"
$Trials  = 10
$Budget  = "8m"
$Workers = 4

New-Item -ItemType Directory -Force -Path $LogDir | Out-Null
Set-Location $Repo

# THE SECOND PREMISE CHECK: the code under test must not change mid-benchmark.
#
# `go run` recompiles from source on every invocation. The first attempt at this
# comparison ran while source files were being edited AND while a mutation script
# was temporarily rewriting them and reverting, so consecutive trials in the same
# arm compiled DIFFERENT programs and some may have compiled deliberately broken
# ones. Six trials of data were discarded for that reason. A fingerprint is
# cheaper than discovering it twice.
# _test.go files are EXCLUDED, and the exclusion is not a loosening to make a
# run pass: `go run ./cmd/thesis` does not compile test files, so a change to one
# cannot change the program under test. The first attempt at this guard included
# them and aborted the run at random seed 3 because an editor touched
# internal/control/parallel_test.go: a file the binary never sees. Everything
# that DOES compile into the binary is still covered byte for byte.
function SourceFingerprint {
    $files = Get-ChildItem -Path (Join-Path $Repo "cmd"), (Join-Path $Repo "internal"), (Join-Path $Repo "pkg") `
        -Recurse -File -Filter *.go |
        Where-Object { $_.Name -notlike "*_test.go" } | Sort-Object FullName
    $sha = [System.Security.Cryptography.SHA256]::Create()
    $acc = [System.Text.StringBuilder]::new()
    foreach ($f in $files) {
        [void]$acc.Append((Get-FileHash -Algorithm SHA256 -LiteralPath $f.FullName).Hash)
    }
    $bytes = [System.Text.Encoding]::UTF8.GetBytes($acc.ToString())
    return ($sha.ComputeHash($bytes) | ForEach-Object { $_.ToString("x2") }) -join ""
}

$fingerprint = SourceFingerprint
Write-Host "source fingerprint: $fingerprint"

"arm,seed,verdict,worlds,seconds,found_consistency,first_violation_ordinal,first_violation_faults,first_violation_seconds,run_id" |
    Out-File -FilePath $Out -Encoding utf8

$seen = @{}

foreach ($arm in @("saboteur", "random")) {
    for ($seed = 1; $seed -le $Trials; $seed++) {
        $log = Join-Path $LogDir "$($arm)_$($seed).log"
        $sw = [Diagnostics.Stopwatch]::StartNew()
        go run ./cmd/thesis search --config $Config --profile search --strategy $arm `
            --workers $Workers --budget $Budget --seed $seed `
            --worker-env "prefix=KV_PREFIX,kv-n1=KV_PORT_N1,kv-n2=KV_PORT_N2,kv-n3=KV_PORT_N3" `
            > $log 2>&1
        $sw.Stop()

        $runDir = Get-ChildItem "$Fixture\.prothesis\runs" -Directory |
            Sort-Object LastWriteTime | Select-Object -Last 1
        $runId = $runDir.Name

        # THE PREMISE CHECK.
        if ($seen.ContainsKey($runId)) {
            throw "[$arm seed $seed] run id $runId already recorded for $($seen[$runId]); this trial produced no run of its own. See $log."
        }
        $seen[$runId] = "$arm seed $seed"

        $now = SourceFingerprint
        if ($now -ne $fingerprint) {
            throw "[$arm seed $seed] the source tree changed mid-benchmark ($fingerprint -> $now). Every trial must compile the same program; discard this run."
        }
        if (-not (Test-Path "$($runDir.FullName)\verdict.json")) {
            throw "[$arm seed $seed] run $runId wrote no verdict.json"
        }

        $worlds = 0; $ord = 0; $nf = 0; $secs = ""
        if (Test-Path "$($runDir.FullName)\search.json") {
            $rec = Get-Content "$($runDir.FullName)\search.json" -Raw | ConvertFrom-Json
            $worlds = $rec.worlds_run; $ord = $rec.first_violation_ordinal
            $nf = $rec.first_violation_fault_count
            if ($null -ne $rec.first_violation_seconds) { $secs = $rec.first_violation_seconds }
        }

        # THE ENDPOINT: a linearizable.kv violation in this run's own verdict.
        $v = Get-Content "$($runDir.FullName)\verdict.json" -Raw | ConvertFrom-Json
        $found = 0
        if ($v.violations | Where-Object { $_.oracle -eq "linearizable.kv" }) { $found = 1 }

        "{0},{1},{2},{3},{4:N1},{5},{6},{7},{8},{9}" -f `
            $arm, $seed, $v.verdict, $worlds, $sw.Elapsed.TotalSeconds, $found, $ord, $nf, $secs, $runId |
            Out-File -FilePath $Out -Encoding utf8 -Append
        Write-Host ("[{0} seed {1}] {2} worlds={3} {4:N0}s consistency={5} run={6}" -f `
            $arm, $seed, $v.verdict, $worlds, $sw.Elapsed.TotalSeconds, $found, $runId)
    }
}
Write-Host "wrote $Out"
