# PRO-THESIS adversarial conformance suite.
#
# WHAT THIS IS FOR, AND WHY THE OTHER SWEEP IS NOT ENOUGH
#
# The end-to-end sweep runs each verb and checks its exit code. It is
# CONFIRMATORY, and it could not have caught a single defect Phase 5 found,
# because all of them had the same shape: a layer reported success while
# measuring nothing.
#
#   * stage 2 reported "6761 -> 1 operations" against a driver that ignored its
#     plan; every candidate reproduced because the workload never changed.
#   * ddmin reported "14 -> 3 faults" having dropped the only essential one,
#     because one lucky world is enough to accept a reduction.
#   * search reported "unknown" while driving 39,009 operations against ports
#     nothing was listening on.
#
# Every one of those exited "correctly". So this suite attacks EPISTEMICS rather
# than plumbing, under two rules:
#
#   1. Never trust the system's own verdict. Re-derive truth from raw artifacts.
#   2. Every probe must be able to FAIL. A probe that can only pass is theatre,
#      and probe A7 exists to prove the others are not.
#
# Usage:  pwsh -File scripts/adversarial.ps1 [-Only A1,A2] [-SkipLive]

param(
    [string[]]$Only = @(),
    [switch]$SkipLive
)

$ErrorActionPreference = 'Continue'
$root    = Split-Path -Parent $PSScriptRoot
$fixture = Join-Path $root 'testdata\kvfixture'
$thesis  = Join-Path $root 'bin\thesis.exe'
$py      = 'python'
$forensics = Join-Path $root 'scripts\adversarial_forensics.py'
$env:GOTMPDIR = Join-Path $root 'bin'

$script:results = @()
function Probe($id, $claim, $whatFailureProves) {
    [pscustomobject]@{ ID = $id; Claim = $claim; Proves = $whatFailureProves }
}
function Record($id, $ok, $detail, $secs) {
    $verdict = if ($ok) { 'PASS' } else { 'FAIL' }
    $script:results += [pscustomobject]@{
        Probe = $id; Result = $verdict; Secs = [Math]::Round($secs,1); Detail = $detail
    }
    Write-Host "[$verdict] $id  $detail"
}
# `pwsh -File script.ps1 -Only A4,A5` passes ONE literal string "A4,A5", not an
# array: -File does no PowerShell parsing of its arguments. Splitting here means
# both -File and dot-sourcing behave the same, which matters because a suite that
# silently runs zero probes and reports success is the exact failure this whole
# file exists to catch.
$script:only = @($Only | ForEach-Object { $_ -split ',' } | ForEach-Object { $_.Trim() } |
    Where-Object { $_ })
function Selected($id) { return ($script:only.Count -eq 0) -or ($script:only -contains $id) }

# latestRun returns the run directory `thesis` just created. Runs are named with
# a timestamp to the minute, so ordering by write time is the only reliable way
# to pick the one that belongs to the command that just finished.
function LatestRun {
    Get-ChildItem (Join-Path $fixture '.prothesis\runs') -Directory |
        Sort-Object LastWriteTime -Descending | Select-Object -First 1 -ExpandProperty FullName
}

Write-Host "=== PRO-THESIS ADVERSARIAL CONFORMANCE SUITE ==="
Write-Host ""

# ---------------------------------------------------------------------------
# A1 - SOUNDNESS. Does the checker accuse an innocent build?
#
# The patched fixture (lease_ms=300) cannot serve a stale read under a partition
# shorter than its lease. Drive it through the SAME mechanism that breaks the
# buggy build, twice over, plus a leader pause.
#
# A failure here is the worst result the suite can produce: if linearizable.kv
# flags a correct build, then every red verdict this system has ever emitted is
# unfalsifiable, the regression corpus is noise, and the Phase 5 acceptance
# evidence means nothing.
# ---------------------------------------------------------------------------
if ((Selected 'A1') -and -not $SkipLive) {
    $t0 = Get-Date
    Push-Location $fixture
    $env:KV_VARIANT = 'kvfixed'
    & $thesis run --profile linear --worlds 3 --seed 90001 `
        --fault 'net.partition(role:leader)@3000..7500' `
        --fault 'net.partition(role:leader)@9000..13000' `
        --fault 'proc.pause(role:leader)@15000..17000' *> $null
    $code = $LASTEXITCODE
    $env:KV_VARIANT = ''
    $run = LatestRun
    Pop-Location
    $out = (& $py $forensics no-violation $run 'linearizable.kv' 2>&1 | Out-String).Trim()
    $ok = $out.StartsWith('OK')
    Record 'A1 innocent-build' $ok "$out (exit $code)" ((Get-Date)-$t0).TotalSeconds
}

# ---------------------------------------------------------------------------
# A2 - NECESSITY. Is the committed regression's fault actually load-bearing?
#
# .prothesis/regressions/w_e952.thesis asserts that ONE fault --
# net.partition(kv-n2)@3000..7500 -- reproduces the defect, and `thesis shrink`
# staked a 3/3 confirmation on it. That claim is falsifiable and has never been
# falsified: nothing in this system runs the world with the fault REMOVED.
#
# So run exactly that world, same seed, same profile, ZERO faults.
#
# A failure here would mean the buggy fixture violates linearizability
# unprovoked, and therefore that: the "no faults -> PASS" step of the e2e sweep
# passed by luck; every ddmin acceptance is confounded; and "minimal repro" names
# a fault that was never necessary. This is not hypothetical -- a noise-only
# fault subset was observed reproducing once during Phase 5 (OQ-053).
# ---------------------------------------------------------------------------
if ((Selected 'A2') -and -not $SkipLive) {
    $t0 = Get-Date
    $worldFile = Join-Path $fixture '.prothesis\regressions\w_e952.thesis'
    $seed = (Get-Content $worldFile -Raw | ConvertFrom-Json).seed
    Push-Location $fixture
    & $thesis run --profile linear --worlds 5 --seed $seed *> $null
    $code = $LASTEXITCODE
    $run = LatestRun
    Pop-Location
    $counts = (& $py $forensics count $run 'linearizable.kv' 2>&1 | Out-String).Trim().Split(' ')
    $hits, $judged = [int]$counts[0], [int]$counts[1]
    $ok = ($hits -eq 0)
    $detail = "zero-fault worlds at the corpus seed: $hits/$judged reproduced"
    if (-not $ok) {
        $detail += " -- the committed regression's fault is NOT necessary; the defect fires unprovoked"
    }
    Record 'A2 necessity' $ok $detail ((Get-Date)-$t0).TotalSeconds
}

# ---------------------------------------------------------------------------
# A3 - RESIDUE. Does HEAL actually restore, after the nastiest pairing?
#
# A kill inside a live partition is the schedule that has broken withdrawal
# before (OQ-042, OQ-048). The claim under test is not the verdict -- it is that
# NOTHING survives: no container, no network, no `thesis:` iptables rule, no
# netem qdisc.
#
# Checked against Docker itself, not against what `down` reported.
# ---------------------------------------------------------------------------
if ((Selected 'A3') -and -not $SkipLive) {
    $t0 = Get-Date
    Push-Location $fixture
    & $thesis run --profile linear --worlds 1 --seed 90003 `
        --fault 'net.partition(role:leader)@3000..9000' `
        --fault 'proc.kill(kv-n3)@5000..7000' *> $null
    $code = $LASTEXITCODE
    & $thesis down *> $null
    Pop-Location
    $containers = @(docker ps -a --filter 'label=io.prothesis.project' --format '{{.Names}}' 2>$null | Where-Object { $_ })
    $networks   = @(docker network ls --filter 'name=thesis-' --format '{{.Name}}' 2>$null | Where-Object { $_ })
    $ok = ($containers.Count -eq 0 -and $networks.Count -eq 0)
    $detail = if ($ok) { "kill-inside-partition left no container or network (exit $code)" }
              else { "RESIDUE: $($containers.Count) container(s), $($networks.Count) network(s)" }
    Record 'A3 residue' $ok $detail ((Get-Date)-$t0).TotalSeconds
}

# ---------------------------------------------------------------------------
# A4 - ANTI-GAMING GATE. Can a covered config value move without exit 4?
#
# Invariant I6 fails CLOSED on oracle drift. perturber.budget is lock-covered, so
# widening it must be refused before anything boots. This is the check that stops
# an agent loosening a gate to make its own run go green.
#
# The mutation is reverted with `git checkout --` whatever happens.
# ---------------------------------------------------------------------------
if (Selected 'A4') {
    $t0 = Get-Date
    $cfg = Join-Path $fixture 'prothesis.yaml'
    $orig = [IO.File]::ReadAllText($cfg)
    try {
        $tampered = $orig.Replace('max_faults_per_world: 24', 'max_faults_per_world: 64')
        if ($tampered -eq $orig) { throw "could not find the value to tamper with" }
        [IO.File]::WriteAllText($cfg, $tampered, (New-Object Text.UTF8Encoding $false))
        Push-Location $fixture
        & $thesis run --profile linear --worlds 1 --seed 90004 *> $null
        $code = $LASTEXITCODE
        Pop-Location
        $ok = ($code -eq 4)
        $detail = if ($ok) { "widening a lock-covered budget was refused with exit 4" }
                  else { "a lock-covered value moved and the run exited $code, not 4 (ORACLE_DRIFT)" }
    } catch {
        $ok = $false; $detail = "probe error: $_"
    } finally {
        [IO.File]::WriteAllText($cfg, $orig, (New-Object Text.UTF8Encoding $false))
    }
    Record 'A4 gate-tamper' $ok $detail ((Get-Date)-$t0).TotalSeconds
}

# ---------------------------------------------------------------------------
# A5 - CONTENT ADDRESSING. Do stored worlds survive a re-encode byte for byte?
#
# Worlds are addressed by SHA-256 over their exact bytes. A re-encode that
# differs by one byte breaks every world_hash at once, and it breaks them in
# someone else's clone rather than here.
# ---------------------------------------------------------------------------
if (Selected 'A5') {
    $t0 = Get-Date
    $pats = @(
        (Join-Path $fixture '.prothesis\regressions\*.thesis'),
        (Join-Path $fixture '.prothesis\runs\*\world-*\world.thesis')
    )
    $out = (& $py $forensics canonical @pats 2>&1 | Out-String).Trim()
    Record 'A5 canonical-bytes' ($out.StartsWith('OK')) $out ((Get-Date)-$t0).TotalSeconds
}

# ---------------------------------------------------------------------------
# A6 - WITNESS INTEGRITY. Does a violation cite evidence that exists?
#
# A verdict naming op ids that appear in no history is not a witness, it is a
# number. This reads the verdict only to extract the CLAIM, then tests it against
# the history the run actually wrote.
# ---------------------------------------------------------------------------
if (Selected 'A6') {
    $t0 = Get-Date
    $runs = Get-ChildItem (Join-Path $fixture '.prothesis\runs') -Directory |
        Sort-Object LastWriteTime -Descending | Select-Object -First 12
    $bad, $checked = @(), 0
    foreach ($r in $runs) {
        $out = (& $py $forensics witness $r.FullName 2>&1 | Out-String).Trim()
        if ($out -match 'verified present') { $checked++ }
        if ($out.StartsWith('FAIL')) { $bad += "$($r.Name): $out" }
    }
    $ok = ($bad.Count -eq 0)
    $detail = if ($ok) { "$checked run(s) with violations: every cited witness exists in its history" }
              else { $bad -join '; ' }
    Record 'A6 witness-integrity' $ok $detail ((Get-Date)-$t0).TotalSeconds
}

# ---------------------------------------------------------------------------
# A7 - THE VERDICT-SIGNING GAP, MEASURED RATHER THAN ASSUMED.
#
# linearizable.kv.yaml documents its own worst limitation in plain words:
#
#   "the lock hashes THIS DEFINITION, not the executable `cmd` names. Swapping
#    ./bin/linearizable-kv for a program that always prints `ok` does not move
#    the digest."
#
# A limitation stated in a comment is a claim, and claims are for testing. This
# probe performs the swap and asks two falsifiable questions:
#
#   A7a  does `thesis oracles verify` still exit 0?      (the gap EXISTS)
#   A7b  does a world KNOWN to reproduce then pass?      (the gap is EXPLOITABLE)
#   A7c  is the swap REPORTED?                           (the gap is not SILENT)
#
# A7a and A7b are measurements and both directions are informative. A7c is a
# PASS/FAIL assertion and it is what gives this probe teeth.
#
# Until D-060 this probe could only ever pass: it measured, restored, and
# recorded PASS whatever it found, so an exploitable verdict-signing hole
# appeared in the summary table as a green row. That was noted honestly in the
# code ("the measurement succeeded") and it was still a reporting hazard, because
# a reader who scans the Result column and not the Detail column sees 7/7.
#
# D-060 makes the swap falsifiable. The lock now records each resolved program's
# SHA-256 OUTSIDE the digest, so:
#
#   * A7a STAYS exit 0 by design. Hashing a binary into the digest would make an
#     ordinary `go build` exit 4 and turn re-locking into a reflex, which is the
#     objection that kept OQ-057 open. The gap is not closed and is not claimed
#     to be.
#   * A7c MUST report the swap. If it does not, D-060 is not doing the one thing
#     it exists to do and this probe FAILS.
#
# A7c needs no Docker, so it runs under -SkipLive where A7b cannot.
#
# NOTE the target. An earlier draft of this probe sabotaged
# cmd/thesis-oracle-linearizable and would have measured nothing, because the
# oracle definition invokes ./bin/linearizable-kv. Attacking the wrong binary and
# reporting "teeth" is precisely the self-deception this suite exists to catch.
# ---------------------------------------------------------------------------
if (Selected 'A7') {
    $t0 = Get-Date
    $realOracle = Join-Path $fixture 'bin\linearizable-kv.exe'
    $backup     = Join-Path $fixture 'bin\linearizable-kv.exe.adversarial-backup'
    $stubSrc    = Join-Path $env:TEMP 'prothesis_stub_oracle.go'
    $ok = $false; $detail = ''
    $realHash = ''
    try {
        if (-not (Test-Path $realOracle)) { throw "oracle binary not found at $realOracle" }
        # OQ-057's write-up claimed the original was "independently verified
        # byte-identical by SHA-256" and this script never computed a hash. It
        # does now: a probe that replaces the program deciding every verdict has
        # to PROVE it put the real one back.
        $realHash = (Get-FileHash $realOracle -Algorithm SHA256).Hash.ToLower()
        Copy-Item $realOracle $backup -Force

        # A checker that answers without checking. It drains STDIN so the engine's
        # write cannot block, then prints a well-formed ok and exits 0 -- exactly
        # the "program that always prints ok" the definition warns about.
        $stub = @'
package main

import (
	"fmt"
	"io"
	"os"
)

func main() {
	_, _ = io.Copy(io.Discard, os.Stdin)
	fmt.Print(`{"schema":"prothesis.oracle_output/v1","oracle":"linearizable.kv",` +
		`"class":"consistency","status":"ok","valid_phases":["ASSERT"],` +
		`"observed_phase":"ASSERT","first_seen_ms":0,` +
		`"explanation":"every key admits a linearization"}`)
	os.Exit(0)
}
'@
        [IO.File]::WriteAllText($stubSrc, $stub, (New-Object Text.UTF8Encoding $false))
        & go build -ldflags '-s -w' -o $realOracle $stubSrc 2>&1 | Out-Null
        if ($LASTEXITCODE -ne 0) { throw "stub oracle failed to build" }

        Push-Location $fixture
        # Captured, not discarded: A7c's whole question is what this PRINTS.
        $verifyOut  = (& $thesis oracles verify 2>&1 | Out-String)
        $verifyCode = $LASTEXITCODE
        $verifyJson = (& $thesis oracles verify --json 2>&1 | Out-String)
        $runCode    = $null
        if (-not $SkipLive) {
            & $thesis run --profile linear --worlds 1 --seed 90007 `
                --fault 'net.partition(role:leader)@3000..7500' *> $null
            $runCode = $LASTEXITCODE
        }
        Pop-Location

        $a7a = if ($verifyCode -eq 0) { "verify still exits 0 (gap EXISTS, by design - D-060)" }
               else { "verify REFUSED the swapped binary (exit $verifyCode) - hashing a binary into the digest is NOT the design" }

        $a7b = if ($null -eq $runCode) { "run skipped (-SkipLive)" }
               elseif ($runCode -eq 0) { "a world known to reproduce came back PASS (gap is EXPLOITABLE)" }
               else { "the run did not pass (exit $runCode)" }

        # A7c. Detection is only meaningful if the lock recorded a fingerprint to
        # compare against. A project locked before D-060 carries none, and
        # failing it for that would punish a project that has simply not been
        # re-locked -- so "not detected" and "nothing to detect against" are
        # distinguished rather than conflated.
        $hasBaseline = $false
        $lockPath = Join-Path $fixture '.prothesis\lock'
        if (Test-Path $lockPath) {
            $lockDoc = Get-Content $lockPath -Raw | ConvertFrom-Json
            $hasBaseline = ($null -ne $lockDoc.executables) -and (@($lockDoc.executables).Count -gt 0)
        }
        $warned = ($verifyOut -match 'WARNING') -and ($verifyOut -match 'linearizable\.kv')
        $inJson = ($verifyJson -match 'executables_moved')

        if (-not $hasBaseline) {
            $a7c = "NOT ASSERTED: .prothesis/lock records no executables block, so there is no " +
                   "fingerprint to compare against. Re-lock to arm this check."
            $ok = $true
        } elseif ($warned -and $inJson) {
            $a7c = "the swap was REPORTED by verify, in text and in --json (the gap is not SILENT)"
            $ok = $true
        } else {
            $a7c = "THE SWAP WAS NOT REPORTED (text=$warned json=$inJson). D-060 records an " +
                   "executable fingerprint precisely so this cannot happen: the program that " +
                   "decides every verdict was replaced and the tool said nothing."
            $ok = $false
        }
        $detail = "$a7a; $a7b; $a7c"
    } catch {
        $ok = $false; $detail = "probe error: $_"
    } finally {
        if (Test-Path $backup) {
            Copy-Item $backup $realOracle -Force
            Remove-Item $backup -Force
        }
        if (Test-Path $stubSrc) { Remove-Item $stubSrc -Force }
        # Restoration is the one thing that must be VERIFIED rather than assumed:
        # this probe deliberately replaced the program that decides whether every
        # future run is honest, and leaving the stub behind would silently turn
        # this repository's whole verdict history green.
        if (-not (Test-Path $realOracle)) {
            $ok = $false
            $detail += "  !! ORACLE BINARY NOT RESTORED - restore it from git before running anything"
        } else {
            # FIRST, byte identity. OQ-057's write-up claimed this check and the
            # script did not perform it; asserting a restore you did not measure
            # is the same class of error the suite exists to catch.
            $restoredHash = (Get-FileHash $realOracle -Algorithm SHA256).Hash.ToLower()
            if ($realHash -and $restoredHash -ne $realHash) {
                $ok = $false
                $detail += ("  !! RESTORE IS NOT BYTE-IDENTICAL: $realHash -> $restoredHash. " +
                            "The checker on disk is NOT the one this probe replaced.")
            } else {
                $detail += "; restore verified byte-identical by SHA-256"
            }

            Push-Location $fixture
            & $thesis oracles verify *> $null
            $sanityCode = $LASTEXITCODE
            # Then prove the restored binary actually CHECKS rather than merely
            # hashing correctly: re-run the world that must reproduce and require
            # a red. Skipped without Docker, where the hash is the whole evidence.
            $restoredRunCode = $null
            if (-not $SkipLive) {
                & $thesis run --profile linear --worlds 2 --seed 90007 `
                    --fault 'net.partition(role:leader)@3000..7500' *> $null
                $restoredRunCode = $LASTEXITCODE
            }
            Pop-Location
            if ($sanityCode -ne 0) {
                $ok = $false
                $detail += "  !! after restore, ``oracles verify`` exits $sanityCode"
            } elseif ($null -ne $restoredRunCode -and $restoredRunCode -ne 1) {
                # The exit code says WHICH failure this is, and an earlier version
                # of this probe threw that away: it attached OQ-054's ~5%
                # flake explanation to every non-1 exit, including exit 2. That is
                # wrong and it was measured to be wrong: a CI run in which the
                # Docker daemon died mid-suite produced exit 2 and this probe
                # blamed the flake, which is a probe naming a cause it had not
                # established. The suite exists to catch exactly that move.
                #
                #   0  both worlds came back clean. THIS is OQ-054's flake: the
                #      world reproduces ~78% of the time, so two independent
                #      misses happen ~5% of runs.
                #   2  a world could not be JUDGED. That is the environment, not
                #      the checker -- a dead daemon, an unbootable topology, an
                #      oracle that could not answer. Re-run before concluding
                #      anything about the restore.
                #   3  the budget expired without a verdict.
                #   4  the lock drifted; 5 the config is invalid.
                #
                # The SHA-256 comparison above is the authoritative evidence that
                # the restore worked, in every one of these cases.
                $why = switch ($restoredRunCode) {
                    0 { "both worlds came back clean, which is OQ-054's ~5% double-miss " +
                        "(p~0.78 per world). Re-run; the hash above already proves the restore." }
                    2 { "a world could not be JUDGED. That is an ENVIRONMENT failure, not " +
                        "evidence about the checker -- check the Docker daemon is alive " +
                        "(OQ-044) before drawing any conclusion." }
                    3 { "the budget expired before a verdict." }
                    default { "exit $restoredRunCode is not a verdict this probe expects." }
                }
                $ok = $false
                $detail += ("  !! after restore, a world that must reproduce exited " +
                            "$restoredRunCode, not 1: $why")
            } else {
                $detail += "; original checker re-verified"
            }
        }
    }
    Record 'A7 verdict-signing-gap' $ok $detail ((Get-Date)-$t0).TotalSeconds
}

# ---------------------------------------------------------------------------
Write-Host ''
Write-Host '=============== ADVERSARIAL RESULTS ==============='
$script:results | Format-Table Probe, Result, Secs, Detail -AutoSize -Wrap
$failed = @($script:results | Where-Object { $_.Result -ne 'PASS' })
$passed = $script:results.Count - $failed.Count
Write-Host ("PROBES: {0} pass, {1} FAIL, {2} run" -f $passed, $failed.Count, $script:results.Count)

# A suite that ran NOTHING must never exit 0. This is the same failure the suite
# exists to hunt -- measuring nothing and reporting success -- and it happened on
# this file's first invocation, when -Only silently matched no probe.
if ($script:results.Count -eq 0) {
    Write-Host "NO PROBES RAN. That is a failure, not a pass: check -Only names against A1..A7."
    exit 2
}
if ($failed.Count -gt 0) { exit 1 }
exit 0
