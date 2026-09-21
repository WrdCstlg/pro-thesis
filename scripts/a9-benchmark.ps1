# A.9 item 4: the pre-registered comparison.
#
# The design is frozen in testdata/kvfixture/.prothesis/PREREGISTRATION.md and
# was written BEFORE this script was run. This file only EXECUTES it; it decides
# nothing.
#
#   two arms, 10 trials each, seeds 1..10 shared
#   endpoint: did the trial produce a linearizable.kv violation within budget
#   one-sided Fisher's exact, alpha 0.05, direction saboteur > random
#
# ---------------------------------------------------------------------------
# WHY THIS SCRIPT ASSERTS ITS OWN PREMISE
#
# The first version of this file did not, and it produced a perfect result
# without running anything. `& $Thesis ...` failed to launch under a redirected
# host with "StandardOutputEncoding is only supported when standard output is
# redirected"; the script then read `.prothesis/runs`, found the PREVIOUS run's
# directory, and recorded its outcome as this trial's. All twenty rows carried
# the same run_id and the same 107.5-second time-to-violation, and the arms tied
# at 10/10: a benchmark that could not fail, reporting a result about code it
# never executed.
#
# So: the run id must be NEW on every trial, and a repeat is a hard abort. That
# single check is what makes the numbers below evidence rather than decoration.
# ---------------------------------------------------------------------------

param(
    [string]$Thesis  = "$env:TEMP\thesis-bench.exe",
    [string]$Fixture = (Join-Path (Split-Path -Parent $PSScriptRoot) "testdata\kvfixture"),
    [string]$Out     = "$env:TEMP\a9_benchmark.csv",
    [string]$LogDir  = "$env:TEMP\a9logs",
    [int]   $Trials  = 10,
    [string]$Budget  = "8m",
    [int]   $Workers = 4
)

$ErrorActionPreference = "Continue"
Set-Location $Fixture
New-Item -ItemType Directory -Force -Path $LogDir | Out-Null

if (-not (Test-Path $Thesis)) { throw "no thesis binary at $Thesis" }

"arm,seed,exit,worlds,seconds,found_consistency,first_violation_ordinal,first_violation_faults,first_violation_seconds,run_id" |
    Out-File -FilePath $Out -Encoding utf8

$seenRuns = @{}

foreach ($arm in @("saboteur", "random")) {
    for ($seed = 1; $seed -le $Trials; $seed++) {

        $stdout = Join-Path $LogDir "$($arm)_$($seed).out"
        $stderr = Join-Path $LogDir "$($arm)_$($seed).err"

        # The child's output is CAPTURED INTO A VARIABLE and written afterwards,
        # rather than redirected with `>`. All three alternatives were tried on
        # this host first and all three fail:
        #
        #   Start-Process           blocked outright by Application Control
        #   cmd.exe /c "... > file" the exe is blocked by Device Guard when
        #                           launched from cmd
        #   & $Thesis ... > file    "StandardOutputEncoding is only supported
        #                           when standard output is redirected", whenever
        #                           THIS script's own stdout is redirected
        #
        # That last one is how the first version of this file produced twenty
        # fabricated rows without launching anything, which is why the run-id
        # check above exists. Do not "simplify" this back to a `>` redirect.
        $sw = [Diagnostics.Stopwatch]::StartNew()
        $captured = & $Thesis search --profile search --strategy $arm --workers $Workers `
            --budget $Budget --seed $seed `
            --worker-env "prefix=KV_PREFIX,kv-n1=KV_PORT_N1,kv-n2=KV_PORT_N2,kv-n3=KV_PORT_N3" 2>&1
        $code = $LASTEXITCODE
        $captured | Out-File -FilePath $stdout -Encoding utf8
        $sw.Stop()

        $runDir = Get-ChildItem "$Fixture\.prothesis\runs" -Directory |
            Sort-Object LastWriteTime | Select-Object -Last 1
        if ($null -eq $runDir) { throw "[$arm seed $seed] no run directory at all" }
        $runId = $runDir.Name

        # THE PREMISE CHECK. A repeated run id means this trial produced no run
        # of its own and the row would be the previous trial's answer wearing
        # this trial's label.
        if ($seenRuns.ContainsKey($runId)) {
            throw ("[$arm seed $seed] run id $runId was already recorded for " +
                   $seenRuns[$runId] + ". The binary did not produce a new run. " +
                   "Check $stdout. Refusing to record a result this trial did not measure.")
        }
        $seenRuns[$runId] = "$arm seed $seed"

        if (-not (Test-Path "$($runDir.FullName)\verdict.json")) {
            throw "[$arm seed $seed] run $runId wrote no verdict.json"
        }

        $worlds = 0; $ord = 0; $nf = 0; $secs = ""
        if (Test-Path "$($runDir.FullName)\search.json") {
            $rec = Get-Content "$($runDir.FullName)\search.json" -Raw | ConvertFrom-Json
            $worlds = $rec.worlds_run
            $ord    = $rec.first_violation_ordinal
            $nf     = $rec.first_violation_fault_count
            if ($null -ne $rec.first_violation_seconds) { $secs = $rec.first_violation_seconds }
        }

        # THE ENDPOINT: a linearizable.kv violation in this run's own verdict.
        $found = 0
        $v = Get-Content "$($runDir.FullName)\verdict.json" -Raw | ConvertFrom-Json
        if ($v.violations | Where-Object { $_.oracle -eq "linearizable.kv" }) { $found = 1 }

        "{0},{1},{2},{3},{4:N1},{5},{6},{7},{8},{9}" -f `
            $arm, $seed, $code, $worlds, $sw.Elapsed.TotalSeconds, $found, $ord, $nf, $secs, $runId |
            Out-File -FilePath $Out -Encoding utf8 -Append

        Write-Host ("[{0} seed {1}] exit={2} worlds={3} {4:N0}s consistency={5} run={6}" -f `
            $arm, $seed, $code, $worlds, $sw.Elapsed.TotalSeconds, $found, $runId)
    }
}

Write-Host "`nwrote $Out"
