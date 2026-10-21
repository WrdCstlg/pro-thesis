# Build every PRO-THESIS binary with build provenance embedded via -ldflags -X.
#
# WHY THIS EXISTS
#
# verdict.json records a commit so an artifact can be traced back to source.
# Before this script existed the harness read the WORKING TREE's HEAD at run
# time: which says where the run happened, not what the binary is: a
# thesis.exe built yesterday from a dirty tree still reported today's HEAD.
# The stamp travels inside the binary, and `thesis version`,
# `thesis-oracle-linearizable -version` and `loadgen -version` read it back.
#
# What each consumer sees:
#   - thesis.exe                verdict.json `commit` = the stamp, falling back
#                               to the tree's HEAD only when unstamped
#   - linearizable-kv.exe       forensic readback via -version
#   - loadgen.exe               forensic readback via -version (fixture module;
#                               its own main.build* vars, same contract)
# The kv node image's provenance is the image digest in sut.images (OQ-055),
# not a stamp.
#
# WHY THE STAMP CARRIES THE COMMIT'S DATE AND NOT THE CLOCK (D-072)
#
# linearizable-kv.exe is fingerprinted by SHA-256 in .prothesis/lock so that a
# swapped checker is visible (D-060, OQ-057). A wall-clock build timestamp put
# those two mechanisms in direct conflict: every rebuild from identical source
# produced different bytes, so the "oracle program changed" warning fired on
# every build, and the honest response to it (re-lock with a reason) decayed
# into a reflex nobody read a diff for. The lock was in fact re-locked twice in
# one day under reasons calling the source unchanged.
#
# So the stamp records SOURCE identity rather than build wall-clock: SourceDate
# is HEAD's committer date, and -trimpath drops the build machine's absolute
# paths. Two builds of the same commit and the same tree are then byte-identical
# (prove it with -Verify), the fingerprint becomes a function of the source, and
# the warning means what it says. Residual, recorded under OQ-057: a commit that
# does not touch the checker still moved its stamp, so a rebuild after any
# commit cost one re-lock.
#
# WHY THE CHECKER, LOADGEN AND TARGET PROGRAMS ARE NOT STAMPED (D-109)
#
# Since D-109 a fingerprinted program that differs from the lock is DRIFT (exit
# 4) on the platform the lock was made on, not a warning. With a commit stamp
# inside, every commit would have changed those bytes and forced a re-lock, and
# a re-lock commit changes HEAD again. So only thesis.exe, which no lock
# fingerprints, carries the stamp. Every program a lock fingerprints (the
# checker, the fixture's loadgen, each target's driver) is built with -trimpath
# -buildvcs=false and nothing else, so its bytes are a function of its source:
# rebuilding unchanged source reproduces the locked digest, and a different
# digest means a different program. Their provenance is that digest, recorded in
# each project's .prothesis/lock.
#
# Every target under targets/ is built here too (OQ-088): its programs into
# targets/<t>/bin, plus a copy of the checker. The Redis health shim runs inside
# a Linux container, so it is cross-compiled.
#
# Usage:
#   pwsh -File scripts/build.ps1
#   pwsh -File scripts/build.ps1 -Verify   # also build a second time and prove
#                                          # the two agree byte for byte

param([switch]$Verify)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

# $ErrorActionPreference does NOT apply to native commands, so every git call
# is checked. `git status` is the one that matters: unguarded it fails OPEN,
# because a non-zero exit with no stdout is falsy and reads exactly like a clean
# tree: stamping Dirty=0, an affirmative claim about the source that nothing
# verified, into the one artifact whose whole job is provenance.
function Invoke-Git {
    param([string[]]$Arguments)
    $out = & git @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "git $($Arguments -join ' ') failed with exit $LASTEXITCODE"
    }
    return $out
}

# Derived by a FUNCTION, and called a second time for the -Verify build. An
# earlier draft computed the stamp once and handed the same frozen string to both
# builds, which made -Verify blind to the one defect it exists to catch: a wall
# clock reintroduced here would be captured once and both builds would agree.
# Deriving it again is what makes the check honest.
function Get-Stamp {
    $commit = (Invoke-Git @('rev-parse', '--short', 'HEAD') | Out-String).Trim()
    if (-not $commit) { throw 'git rev-parse --short HEAD produced no revision' }

    $dirty = if (Invoke-Git @('status', '--porcelain')) { '1' } else { '0' }

    # %cI is the committer date in strict ISO 8601. Parsed and formatted with the
    # invariant culture on purpose: ToString() under a non-Gregorian calendar
    # (th-TH renders 2026 as 2569) would stamp a date that is not the commit's.
    $rawDate = (Invoke-Git @('show', '-s', '--format=%cI', 'HEAD') | Out-String).Trim()
    if (-not $rawDate) { throw 'git show -s --format=%cI HEAD produced no date' }
    $sourceDate = [datetimeoffset]::Parse($rawDate, [cultureinfo]::InvariantCulture).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ', [cultureinfo]::InvariantCulture)

    return [pscustomobject]@{ Commit = $commit; Dirty = $dirty; SourceDate = $sourceDate }
}

$pkg = 'github.com/WrdCstlg/pro-thesis/internal/buildinfo'
function Format-HarnessLdflags {
    param($Stamp)
    "-X $pkg.Commit=$($Stamp.Commit) -X $pkg.Dirty=$($Stamp.Dirty) -X $pkg.SourceDate=$($Stamp.SourceDate)"
}

# -buildvcs=false because Go's own VCS stamping defeats the whole point here:
# it embeds vcs.revision, vcs.time and vcs.modified independently of -ldflags, so
# adding ONE unrelated untracked file to a clean tree flips vcs.modified and moves
# the oracle's digest; measured. The lock would then report a changed checker
# because someone left a scratch file lying about. The provenance is not lost:
# commit, dirty and source date are stamped explicitly above and are readable
# from the binary with `go version -m` and each binary's version flag.
$commonBuildArgs = @('-trimpath', '-buildvcs=false')

$stamp = Get-Stamp
$commit = $stamp.Commit
$dirty = $stamp.Dirty
$sourceDate = $stamp.SourceDate
$ld = Format-HarnessLdflags $stamp

# A failed go build is silent without an explicit $LASTEXITCODE check, and a
# provenance script that reports success over a partial or failed build is worse
# than no script.
function Invoke-Build {
    param([string[]]$Arguments, [string]$WorkDir)
    Push-Location $WorkDir
    try {
        & go build @Arguments
        if ($LASTEXITCODE -ne 0) { throw "go build $($Arguments -join ' ') failed with exit $LASTEXITCODE in $WorkDir" }
    }
    finally { Pop-Location }
}

Write-Host "stamping commit=$commit dirty=$dirty source_date=$sourceDate"

$oracleOut = 'testdata/kvfixture/bin/linearizable-kv.exe'
Invoke-Build ($commonBuildArgs + @('-ldflags', $ld, '-o', 'bin/thesis.exe', './cmd/thesis')) $root
# Unstamped: fingerprinted by the locks (see the header).
Invoke-Build ($commonBuildArgs + @('-o', $oracleOut, './cmd/thesis-oracle-linearizable')) $root
Invoke-Build ($commonBuildArgs + @('-o', 'bin/loadgen.exe', './cmd/loadgen')) (Join-Path $root 'testdata/kvfixture')
$built = @('bin/thesis.exe', $oracleOut, 'testdata/kvfixture/bin/loadgen.exe')

# Each target: every program under targets/<t>/cmd, and the checker its locked
# oracle definition names, copied byte for byte.
foreach ($t in Get-ChildItem (Join-Path $root 'targets') -Directory) {
    $cmdDir = Join-Path $t.FullName 'cmd'
    if (-not (Test-Path $cmdDir)) { continue }
    $bin = Join-Path $t.FullName 'bin'
    New-Item -ItemType Directory -Force $bin | Out-Null
    foreach ($prog in Get-ChildItem $cmdDir -Directory) {
        $pkgPath = './targets/' + $t.Name + '/cmd/' + $prog.Name
        if ($prog.Name -eq 'redishealth') {
            # Runs inside a Linux container, mounted read-only (targets/redis).
            $savedOS, $savedCGO = $env:GOOS, $env:CGO_ENABLED
            try {
                $env:GOOS = 'linux'; $env:CGO_ENABLED = '0'
                Invoke-Build ($commonBuildArgs + @('-o', "targets/$($t.Name)/bin/$($prog.Name)", $pkgPath)) $root
            }
            finally { $env:GOOS = $savedOS; $env:CGO_ENABLED = $savedCGO }
            $built += "targets/$($t.Name)/bin/$($prog.Name) (linux)"
            continue
        }
        Invoke-Build ($commonBuildArgs + @('-o', "targets/$($t.Name)/bin/$($prog.Name).exe", $pkgPath)) $root
        $built += "targets/$($t.Name)/bin/$($prog.Name).exe"
    }
    Copy-Item -LiteralPath (Join-Path $root $oracleOut) -Destination (Join-Path $bin 'linearizable-kv.exe') -Force
    $built += "targets/$($t.Name)/bin/linearizable-kv.exe (copy)"
}

Write-Host ("built:`n  " + ($built -join "`n  "))

if ($Verify) {
    # The property the locks depend on: same source, same bytes. The second
    # builds go to scratch paths so a mismatch cannot overwrite what was just
    # shipped.
    foreach ($v in @(
            @{ Out = $oracleOut; Pkg = './cmd/thesis-oracle-linearizable'; Dir = $root },
            @{ Out = 'testdata/kvfixture/bin/loadgen.exe'; Pkg = './cmd/loadgen'; Dir = (Join-Path $root 'testdata/kvfixture') })) {
        $probe = Join-Path ([IO.Path]::GetTempPath()) ('prothesis-verify-' + [IO.Path]::GetFileName($v.Out))
        Invoke-Build ($commonBuildArgs + @('-o', $probe, $v.Pkg)) $v.Dir
        $shipped = (Get-FileHash (Join-Path $root $v.Out) -Algorithm SHA256).Hash
        $second = (Get-FileHash $probe -Algorithm SHA256).Hash
        try { [IO.File]::Delete($probe) } catch { }
        if ($shipped -ne $second) {
            throw "build is NOT reproducible: $($v.Out) = $shipped, second build = $second. A lock fingerprints this binary, so a non-reproducible build makes every rebuild indistinguishable from a swapped program."
        }
        Write-Host "verified reproducible: $($v.Out) agrees on sha256:$($shipped.ToLower())"
    }
}

if ($dirty -eq '1') {
    Write-Host 'note: the tree is dirty, so the thesis.exe stamp names a commit it only approximately matches.'
}
Write-Host 'note: a fingerprinted program whose digest moved is drift on the locking platform (D-109); if its'
Write-Host '      source changed on purpose, re-lock with a reason that names the change.'
