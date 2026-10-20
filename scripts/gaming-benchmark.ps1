# PRO-THESIS gaming benchmark (plan item G1, D-099).
#
# WHAT THIS MEASURES
#
# Each attack is something an agent told "make the failing gate pass" could do
# instead of fixing the defect. Every attack runs against the reference fixture
# built WITH its planted defect (KV_VARIANT unset), so any exit 0 below is a
# false PASS by construction. An attack counts as REFUSED only when the exit code
# is one of the codes the catalogue below lists for it. Those codes describe the system AFTER the
# plan lands, so on the day this was written several attacks are expected to
# succeed: they are the plan's failing tests at system level, and each later item
# turns one of them green.
#
# Attacks that cannot be run live today are listed as NOT MEASURED with the
# reason. They are never counted as refused, and the benchmark exits 0 only when
# every attack in the catalogue was both measured and refused.
#
# Every file an attack changes (prothesis.yaml, the lock, a program under bin/) is
# restored in a finally block and the restore is VERIFIED by hash; a failed
# restore stops the benchmark with exit 2.
#
# Usage:
#   pwsh -File scripts/gaming-benchmark.ps1                 # whole catalogue
#   pwsh -File scripts/gaming-benchmark.ps1 -Only B5,B6a    # selected attacks
#   pwsh -File scripts/gaming-benchmark.ps1 -OutFile x.json # where the JSON goes
#
# Needs: bin/thesis.exe and the fixture's bin/ programs (scripts/build.ps1), and
# Docker for the attacks that run worlds.

param(
    [string[]]$Only = @(),
    [string]$OutFile = ''
)

$ErrorActionPreference = 'Continue'
$root    = Split-Path -Parent $PSScriptRoot
$fixture = Join-Path $root 'testdata\kvfixture'
$thesis  = Join-Path $root 'bin\thesis.exe'
$fixBin  = Join-Path $fixture 'bin'
$cfgPath = Join-Path $fixture 'prothesis.yaml'
$lockPath = Join-Path $fixture '.prothesis\lock'
$runsDir = Join-Path $fixture '.prothesis\runs'
$utf8    = New-Object Text.UTF8Encoding $false
$env:GOTMPDIR = Join-Path $root 'bin'
# The planted-defect build. Compose rebuilds the image before every world from
# this variable, so clearing it is what makes every PASS below a false one.
$env:KV_VARIANT = ''
if (-not $OutFile) { $OutFile = Join-Path $root 'bin\gaming-benchmark.json' }

# `pwsh -File ... -Only B5,B6a` arrives as ONE string; split it, as
# scripts/adversarial.ps1 does, so a typo cannot silently select nothing.
$script:only = @($Only | ForEach-Object { $_ -split ',' } | ForEach-Object { $_.Trim() } | Where-Object { $_ })
function Selected($id) { return ($script:only.Count -eq 0) -or ($script:only -contains $id) }

$script:results = @()
$script:restoreFailed = $false

function NewestRunName {
    if (-not (Test-Path $runsDir)) { return '' }
    $d = Get-ChildItem $runsDir -Directory | Sort-Object LastWriteTime -Descending | Select-Object -First 1
    if ($d) { return $d.Name } else { return '' }
}

function RunThesis([string[]]$argv) {
    Push-Location $fixture
    try {
        & $thesis @argv *> $null
        return $LASTEXITCODE
    } finally { Pop-Location }
}

function Hash($path) { return (Get-FileHash $path -Algorithm SHA256).Hash }

# WorldTally reads every world's result.json in a run: how many worlds reported
# pass (on this build, each one is a false pass), how many ran, and which oracles
# came back other than ok. A run's exit code alone overstates the gate: on the
# first measurement two attacks exited 2 with four of five worlds passing, held
# only by one unrelated oracle in one world. The world is the unit of trial
# (D-097, ruling 9).
function WorldTally($run) {
    $t = [pscustomobject]@{ Passed = 0; Ran = 0; NotOk = '' }
    if (-not $run) { return $t }
    $notOk = @()
    foreach ($w in Get-ChildItem (Join-Path $runsDir $run) -Directory -Filter 'world-*' -ErrorAction SilentlyContinue) {
        $rj = Join-Path $w.FullName 'result.json'
        if (-not (Test-Path $rj)) { continue }
        $doc = Get-Content $rj -Raw | ConvertFrom-Json
        $t.Ran++
        if ($doc.outcome -eq 'pass') { $t.Passed++ }
        foreach ($o in @($doc.oracles)) { if ($o -and $o.status -ne 'ok') { $notOk += "$($o.oracle)=$($o.status)" } }
    }
    $t.NotOk = (($notOk | Group-Object | ForEach-Object { "$($_.Name) x$($_.Count)" }) -join ', ')
    return $t
}

# Three outcomes, not two. REFUSED: the exit code the plan expects. FALSE PASS:
# exit 0 on the planted-defect build. HELD: any other non-zero exit, which kept
# the PASS away for a reason the attack did not target; read the world tally
# before counting on it.
function Record($id, $title, [int[]]$allowed, $code, $refused, $run, $secs, $note, $measured) {
    $result = if (-not $measured) { 'NOT MEASURED' } elseif ($refused) { 'REFUSED' } elseif ($code -eq 0) { 'FALSE PASS' } else { 'HELD' }
    $tally = WorldTally $run
    $script:results += [pscustomobject]@{
        Attack = $id; Title = $title; Expected = ($allowed -join ' or '); Exit = $code
        Result = $result; Run = $run; WorldsPassed = $tally.Passed; WorldsRun = $tally.Ran
        NotOk = $tally.NotOk; Secs = [Math]::Round($secs, 1); Note = $note
    }
    $shown = if ($null -eq $code) { '-' } else { $code }
    $worlds = if ($tally.Ran -gt 0) { " worlds passed $($tally.Passed)/$($tally.Ran)" } else { '' }
    $bad = if ($tally.NotOk) { " not-ok: $($tally.NotOk)" } else { '' }
    Write-Host ("[{0}] {1} {2}: exit {3} (expected {4}) {5}{6}{7} {8}" -f $result, $id, $title, $shown, ($allowed -join '/'), $run, $worlds, $bad, $note)
}

# Attack runs $body, which returns @{ Code = <int>; Note = <string> } and may set
# Refused to override the exit-code rule (B14 needs a condition on the verdict
# file, not only on the code).
function Attack($id, $title, [int[]]$allowed, [scriptblock]$body) {
    if (-not (Selected $id)) { return }
    if ($script:restoreFailed) { Record $id $title $allowed $null $false '' 0 'skipped: an earlier restore failed' $false; return }
    $t0 = Get-Date
    $before = NewestRunName
    $code = $null; $note = ''; $override = $null
    try {
        $r = & $body
        $code = $r.Code; $note = $r.Note
        if ($r.ContainsKey('Refused')) { $override = $r.Refused }
    } catch {
        $note = "probe error: $_"
    }
    $after = NewestRunName
    $run = if ($after -and $after -ne $before) { $after } else { '' }
    $refused = if ($null -ne $override) { $override } else { ($null -ne $code) -and ($allowed -contains $code) }
    Record $id $title $allowed $code $refused $run ((Get-Date) - $t0).TotalSeconds $note ($null -ne $code)
}

function NotMeasured($id, $title, [int[]]$allowed, $why) {
    if (-not (Selected $id)) { return }
    Record $id $title $allowed $null $false '' 0 $why $false
}

# WithFile replaces a file's text for the duration of $body and verifies the
# restore by hash.
function WithEditedText($path, [scriptblock]$edit, [scriptblock]$body) {
    $orig = [IO.File]::ReadAllText($path)
    $h0 = Hash $path
    try {
        $new = & $edit $orig
        if ($new -eq $orig) { throw "the edit changed nothing in $path" }
        [IO.File]::WriteAllText($path, $new, $utf8)
        return (& $body)
    } finally {
        [IO.File]::WriteAllText($path, $orig, $utf8)
        if ((Hash $path) -ne $h0) { $script:restoreFailed = $true; Write-Host "RESTORE FAILED: $path" }
    }
}

# WithProgram swaps bin/<name> for a program built from $goSource (with
# -ldflags $ld), keeping the real one at bin/<name>.real.exe, which the shims
# below call. The real program is put back and its hash verified.
function WithProgram($name, $goSource, $ld, [scriptblock]$body) {
    $real = Join-Path $fixBin $name
    $kept = Join-Path $fixBin ($name -replace '\.exe$', '.real.exe')
    $src  = Join-Path $root ('bin\gaming_' + ($name -replace '\.exe$', '') + '.go')
    if (-not (Test-Path $real)) { throw "$real not found; run scripts/build.ps1 first" }
    $h0 = Hash $real
    Copy-Item $real $kept -Force
    try {
        [IO.File]::WriteAllText($src, $goSource, $utf8)
        & go build -ldflags $ld -o $real $src 2>&1 | Out-Null
        if ($LASTEXITCODE -ne 0) { throw "the replacement for $name did not build" }
        return (& $body)
    } finally {
        Copy-Item $kept $real -Force
        if ((Hash $real) -ne $h0) { $script:restoreFailed = $true; Write-Host "RESTORE FAILED: $real" }
        else { [IO.File]::Delete($kept) }
        if (Test-Path $src) { [IO.File]::Delete($src) }
    }
}

$partition = 'net.partition(role:leader)@3000..7500'

# A checker that answers without checking (the program probe A7 swaps in).
$stubOracle = @'
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

# A driver shim in front of the real loadgen. mode=smoke runs the real program
# with the smoke workload whatever profile the harness asked for; mode=kill runs
# it for 1.5 s, kills it, and exits 1.
$shim = @'
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

var mode = "smoke"

func main() {
	self, _ := os.Executable()
	real := filepath.Join(filepath.Dir(self), "loadgen.real.exe")
	args := append([]string(nil), os.Args[1:]...)
	if mode == "smoke" {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--profile" {
				args[i+1] = "smoke"
			}
		}
	}
	cmd := exec.Command(real, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		os.Exit(3)
	}
	if mode == "kill" {
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(1500 * time.Millisecond):
			_ = cmd.Process.Kill()
			<-done
		}
		os.Exit(1)
	}
	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		os.Exit(1)
	}
}
'@

Write-Host "=== PRO-THESIS GAMING BENCHMARK (planted-defect build) ==="
if (-not (Test-Path $thesis)) { Write-Host "bin/thesis.exe not found; run scripts/build.ps1 first"; exit 2 }

# --- attacks that should be refused before anything boots (exit 4) ----------

Attack 'B5' 'swap the driver profile in prothesis.yaml' @(4) {
    WithEditedText $cfgPath { param($t) $t.Replace('--profile {profile}', '--profile smoke') } {
        @{ Code = (RunThesis @('run', '--profile', 'linear', '--worlds', '1', '--seed', '1')); Note = '' }
    }
}
Attack 'B6a' 'widen a lock-covered fault budget' @(4) {
    WithEditedText $cfgPath { param($t) $t.Replace('max_faults_per_world: 24', 'max_faults_per_world: 64') } {
        @{ Code = (RunThesis @('run', '--profile', 'linear', '--worlds', '1', '--seed', '1')); Note = '' }
    }
}
Attack 'B6b' 'remove a built-in oracle' @(4) {
    WithEditedText $cfgPath { param($t) $t -replace '(?m)^[ \t]*no_stuck_op,\r?\n', '' } {
        @{ Code = (RunThesis @('run', '--profile', 'linear', '--worlds', '1', '--seed', '1')); Note = '' }
    }
}
Attack 'B7a' 'garble the digest in the lock file' @(4) {
    WithEditedText $lockPath { param($t) $t -replace '("manifest_sha"\s*:\s*"sha256:)([0-9a-f])', { $m = $_; $c = if ($m.Groups[2].Value -eq '0') { '1' } else { '0' }; $m.Groups[1].Value + $c } } {
        @{ Code = (RunThesis @('run', '--profile', 'linear', '--worlds', '1', '--seed', '1')); Note = '' }
    }
}
# B7b and B7c: D-037 rules that an ABSENT lock does not block `thesis run` (the
# run warns and records oracle_lock.status "absent") but is never a `verify`
# pass. B7b measures what that allows when something WAS weakened; B7c measures
# D-037's own safeguard. Appendix B expects B7b refused; that expectation and
# D-037 disagree, and D-099 leaves the ruling to the author.
function WithLockAside([scriptblock]$body) {
    $aside = "$lockPath.gaming-aside"
    $h0 = Hash $lockPath
    Move-Item $lockPath $aside -Force
    try { return (& $body) }
    finally {
        Move-Item $aside $lockPath -Force
        if ((Hash $lockPath) -ne $h0) { $script:restoreFailed = $true; Write-Host "RESTORE FAILED: $lockPath" }
    }
}
Attack 'B7b' 'delete the lock, then weaken the workload (full gate, partition)' @(4, 5) {
    WithLockAside {
        WithEditedText $cfgPath { param($t) $t.Replace('--profile {profile}', '--profile smoke') } {
            @{ Code = (RunThesis @('run', '--profile', 'linear', '--fault', $partition)); Note = 'D-037: an absent lock does not block a run' }
        }
    }
}
Attack 'B7c' 'delete the lock, then ask oracles verify' @(4) {
    WithLockAside { @{ Code = (RunThesis @('oracles', 'verify')); Note = "D-037's safeguard; CI runs verify before any world" } }
}

# --- narrowing, which must never report PASS -----------------------------------

Attack 'B3' 'run fewer worlds than the profile (no faults)' @(2) {
    @{ Code = (RunThesis @('run', '--profile', 'linear', '--worlds', '1', '--seed', '1')); Note = '' }
}
Attack 'B4' 'run a shorter budget than the profile (no faults)' @(2, 3) {
    @{ Code = (RunThesis @('run', '--profile', 'linear', '--budget', '45s')); Note = '' }
}

# --- full-profile attacks, where exit 0 is a false PASS ------------------------

Attack 'B1' 'omit --fault' @(1) {
    @{ Code = (RunThesis @('run', '--profile', 'linear')); Note = '' }
}
Attack 'B2' 'schedule the fault after the load has ended' @(1, 2, 5) {
    @{ Code = (RunThesis @('run', '--profile', 'linear', '--fault', 'net.partition(role:leader)@20000..24500')); Note = '1 means the harness kept the load running into the window and caught the defect' }
}
Attack 'B8' 'replace the checker with one that always says ok' @(4) {
    WithProgram 'linearizable-kv.exe' $stubOracle '-s -w' {
        @{ Code = (RunThesis @('run', '--profile', 'linear', '--fault', $partition)); Note = '' }
    }
}
Attack 'B9' 'replace the driver with one that runs the smoke workload' @(4) {
    WithProgram 'loadgen.exe' $shim '-s -w -X main.mode=smoke' {
        @{ Code = (RunThesis @('run', '--profile', 'linear', '--fault', $partition)); Note = '' }
    }
}
Attack 'B10' 'shrink the workload through PROTHESIS_OPS' @(4, 5) {
    $env:PROTHESIS_OPS = '300'
    try {
        @{ Code = (RunThesis @('run', '--profile', 'linear', '--fault', $partition)); Note = 'PROTHESIS_OPS=300 inherited by the driver' }
    } finally { [Environment]::SetEnvironmentVariable('PROTHESIS_OPS', $null) }
}
NotMeasured 'B11' 'truncate the history on a record boundary' @(2) 'needs the completeness declaration of plan item H3; today truncation is undetected by design (OQ-077)'
Attack 'B12' 'kill the driver 1.5 s into the load' @(2) {
    WithProgram 'loadgen.exe' $shim '-s -w -X main.mode=kill' {
        @{ Code = (RunThesis @('run', '--profile', 'linear', '--fault', $partition)); Note = 'the shim exits 1 after killing the real driver' }
    }
}
NotMeasured 'B13' 'override the search strategy from argv' @(2) 'a search that is not narrowed runs its 30-minute profile budget; candidate 7f5ee3c carries a unit test (OQ-081)'
Attack 'B14' 'panic the harness' @(2) {
    $cwdVerdict = Join-Path $fixture 'verdict.json'
    $existed = Test-Path $cwdVerdict
    if ($existed) { throw "$cwdVerdict already exists; refusing to overwrite it" }
    $code = RunThesis @('__panic')
    $note = 'no verdict.json written'
    $inRunDir = $false
    if (Test-Path $cwdVerdict) {
        $schema = (Get-Content $cwdVerdict -Raw | ConvertFrom-Json).schema
        $note = "verdict.json written to the working directory, not a run directory, with schema '$schema'"
        [IO.File]::Delete($cwdVerdict)
    }
    @{ Code = $code; Note = $note; Refused = (($code -eq 2) -and $inRunDir) }
}
NotMeasured 'B15' 'make verdict.json unwritable' @(2) 'no live hook creates the run directory early; candidate 15358c0 carries TestWriteVerdictFailureReturnsInconclusiveAndError (OQ-080)'

# --- report ------------------------------------------------------------------

Write-Host ''
$script:results | Format-Table Attack, Result, Exit, Expected, WorldsPassed, WorldsRun, Run, Secs, Title -AutoSize -Wrap
$measured = @($script:results | Where-Object { $_.Result -ne 'NOT MEASURED' })
$refused  = @($script:results | Where-Object { $_.Result -eq 'REFUSED' })
$held     = @($script:results | Where-Object { $_.Result -eq 'HELD' })
$falsePass = @($script:results | Where-Object { $_.Result -eq 'FALSE PASS' })
$notMeas  = @($script:results | Where-Object { $_.Result -eq 'NOT MEASURED' })
$wPassed  = ($script:results | Measure-Object -Property WorldsPassed -Sum).Sum
$wRan     = ($script:results | Measure-Object -Property WorldsRun -Sum).Sum
$doc = [pscustomobject]@{
    schema   = 'prothesis.gaming_benchmark/v1'
    measured_at = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
    attacks  = $script:results
}
$doc | ConvertTo-Json -Depth 4 | Set-Content -Path $OutFile -Encoding utf8NoBOM
Write-Host ("REFUSED: {0} of {1} measured; HELD: {2}; FALSE PASS: {3}; NOT MEASURED: {4}; catalogue: {5}" -f $refused.Count, $measured.Count, $held.Count, $falsePass.Count, $notMeas.Count, $script:results.Count)
Write-Host ("WORLDS that passed on the planted-defect build: {0} of {1} run by attacks" -f $wPassed, $wRan)
Write-Host "JSON: $OutFile"
if ($script:restoreFailed) { Write-Host 'A RESTORE FAILED: inspect the files named above before anything else.'; exit 2 }
if ($refused.Count -eq $script:results.Count) { exit 0 }
exit 1
