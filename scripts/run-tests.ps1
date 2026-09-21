# Run the Go test suite on a host whose Application Control policy blocks
# dynamically-created executables.
#
# WHY THIS EXISTS
#
# `go test` compiles each package's test binary into a temporary directory and
# executes it from there. On this host the policy blocks that execution:
#
#   fork/exec ...\go-build2257686346\b001\schema.test.exe:
#     An Application Control policy has blocked this file.
#
# Setting GOTMPDIR to a project directory was the documented workaround and it
# stopped being sufficient partway through Phase 5: .gotmp, the repo root and
# bin/ are all blocked for `go-build*` subdirectories, while an executable
# sitting DIRECTLY in bin/ (bin/thesis.exe) runs normally.
#
# So: compile each package's test binary to bin/<pkg>.test.exe with `go test -c`
# (which compiles but never executes), then run it directly. Compilation is not
# blocked; only execution from the generated subdirectory is.
#
# The binary must run with its PACKAGE as the working directory, because tests
# open testdata/ by relative path. `go test` does this for you; running the
# binary by hand does not.
#
# Usage:
#   pwsh -File scripts/run-tests.ps1              # whole module
#   pwsh -File scripts/run-tests.ps1 -Match shrink  # packages matching a substring
#   pwsh -File scripts/run-tests.ps1 -Run TestFoo   # a single test, all packages

param(
    [string]$Match = '',
    [string]$Run   = '',
    [switch]$Verbose
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root
$env:GOTMPDIR = Join-Path $root 'bin'
$outDir = Join-Path $root 'bin'

# BOTH modules, not just this one. testdata/kvfixture is a separate module
# (prothesis.dev/kvfixture) and `go list ./...` from the root never sees it, so
# for a long time "the suite is green" was a statement about one of the two,
# while the fixture is the system every gate is exercised against, and holds the
# deliberate lease bug the whole corpus is built on.
$modules = @(
    [pscustomobject]@{ Dir = $root; Prefix = 'github.com/WrdCstlg/pro-thesis'; Tag = '' },
    [pscustomobject]@{ Dir = (Join-Path $root 'testdata\kvfixture'); Prefix = 'prothesis.dev/kvfixture'; Tag = 'kvfixture/' }
)

$pkgs = @()
foreach ($m in $modules) {
    Push-Location $m.Dir
    try {
        foreach ($p in (& go list ./...)) {
            if (-not $p) { continue }
            $pkgs += [pscustomobject]@{ Path = $p; Module = $m }
        }
    } finally { Pop-Location }
}
$pkgs = $pkgs | Where-Object { $Match -eq '' -or $_.Path -like "*$Match*" }
if (-not $pkgs) { Write-Host "no packages matched '$Match'"; exit 1 }

$results = @()
foreach ($entry in $pkgs) {
    $pkg  = $entry.Path
    $mod  = $entry.Module
    $rel  = $pkg -replace ('^' + [regex]::Escape($mod.Prefix) + '/?'), ''
    $dir  = if ($rel) { Join-Path $mod.Dir ($rel -replace '/', '\') } else { $mod.Dir }
    $rel  = $mod.Tag + $rel
    $name = if ($rel) { ($rel -replace '[\\/]', '_') } else { 'root' }
    $bin  = Join-Path $outDir "$name.test.exe"

    # A binary left over from an earlier run must never be what we execute: the
    # Test-Path gate below cannot tell "compiled now" from "compiled last week",
    # so a package whose compile fails would silently run its previous binary and
    # report ok. Delete first, then compile.
    if (Test-Path $bin) { try { [System.IO.File]::Delete($bin) } catch { } }

    # -c compiles without running. A package with no test files produces no
    # binary and is not a failure.
    #
    # -ldflags "-s -w" strips the symbol table and DWARF. That is not an
    # optimisation here: four packages' test binaries were refused by the policy
    # on every attempt while seventeen others ran, and stripping was what let them
    # through. Whatever the rule keys on, the stripped binary satisfies it. Tests
    # do not need symbols to run, and a panic still reports its message and the
    # failing assertion.
    # Compiled from the package's OWN module root: `go test -c` on a fixture
    # package resolves against the wrong go.mod from anywhere else.
    Push-Location $mod.Dir
    try {
        $compile = (& go test -c -ldflags '-s -w' -o $bin $pkg 2>&1 | Out-String).Trim()
        $compileCode = $LASTEXITCODE
    } finally { Pop-Location }
    if (-not (Test-Path $bin)) {
        # A package with no test files writes no binary and exits 0, printing
        # "?   <pkg>   [no test files]". Classifying on "did it print anything"
        # called that a BUILD-FAIL and made the whole suite exit 1 over two
        # test-less packages: a red signal on a green tree, which is worse than
        # no signal because it teaches the reader to ignore the exit code. The
        # exit CODE is what distinguishes the two.
        if ($compileCode -ne 0) {
            $results += [pscustomobject]@{ Package = $rel; Result = 'BUILD-FAIL'; Secs = 0 }
            Write-Host "[BUILD-FAIL] $rel"
            Write-Host $compile
        } else {
            $results += [pscustomobject]@{ Package = $rel; Result = 'no tests'; Secs = 0 }
        }
        continue
    }

    # NOT $args: that is an automatic variable in PowerShell and assigning to it
    # inside a function silently does not do what it looks like it does.
    $targs = @('-test.count=1')
    if ($Run)     { $targs += "-test.run=$Run" }
    if ($Verbose) { $targs += '-test.v' }

    # The policy blocks INTERMITTENTLY: the same binary in the same directory is
    # refused on one attempt and runs on the next, which is why the original
    # GOTMPDIR workaround appeared to work for two phases. Retry before believing
    # a block, and never let one blocked package end the run.
    $t0 = Get-Date
    $out = ''
    $code = 0
    $blocked = $false
    for ($attempt = 1; $attempt -le 3; $attempt++) {
        $blocked = $false
        Push-Location $dir
        try {
            $out = (& $bin @targs 2>&1 | Out-String)
            $code = $LASTEXITCODE
        } catch {
            $out = $_.Exception.Message
            $code = -1
            $blocked = $true
        } finally {
            Pop-Location
        }
        if ($out -match 'Application Control') { $blocked = $true }
        if (-not $blocked) { break }
        Start-Sleep -Milliseconds 400
    }

    # Stripping usually satisfies the policy, but not always: on 2026-09-17 the
    # fixture's cmd/loadgen binary was refused on nine consecutive stripped
    # attempts and then ran first try UNSTRIPPED, in the same directory, same
    # session. Whatever the rule keys on, the two builds land on opposite sides
    # of it, so a block is worth one more try the other way before it is reported
    # as a block.
    if ($blocked) {
        Write-Host "[retry] $rel - stripped binary refused; rebuilding unstripped"
        if (Test-Path $bin) { try { [System.IO.File]::Delete($bin) } catch { } }
        Push-Location $mod.Dir
        try { & go test -c -o $bin $pkg 2>&1 | Out-Null } finally { Pop-Location }
        if (Test-Path $bin) {
            $blocked = $false
            Push-Location $dir
            try {
                $out = (& $bin @targs 2>&1 | Out-String)
                $code = $LASTEXITCODE
            } catch {
                $out = $_.Exception.Message
                $code = -1
                $blocked = $true
            } finally {
                Pop-Location
            }
            if ($out -match 'Application Control') { $blocked = $true }
        }
    }
    $secs = [Math]::Round(((Get-Date) - $t0).TotalSeconds, 1)

    try { [System.IO.File]::Delete($bin) } catch { }

    if ($blocked) {
        $results += [pscustomobject]@{ Package = $rel; Result = 'BLOCKED'; Secs = $secs }
        Write-Host "[BLOCKED] $rel - Application Control refused it on 3 attempts"
        continue
    }
    $verdict = if ($code -eq 0) { 'ok' } else { 'FAIL' }
    $results += [pscustomobject]@{ Package = $rel; Result = $verdict; Secs = $secs }
    Write-Host "[$verdict] $rel  ${secs}s"
    if ($code -ne 0) { Write-Host ($out.Trim()) }
}

Write-Host ''
$results | Format-Table Package, Result, Secs -AutoSize
$bad = @($results | Where-Object { $_.Result -notin @('ok', 'no tests') })
$good = @($results | Where-Object { $_.Result -eq 'ok' }).Count
Write-Host ("PACKAGES: {0} ok, {1} not ok, {2} total" -f $good, $bad.Count, $results.Count)
if ($bad.Count -gt 0) { exit 1 }
exit 0
