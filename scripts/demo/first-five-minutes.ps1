# Record the README's "first five minutes" against the fixture, exactly as they run.
#
#   pwsh -File scripts/demo/first-five-minutes.ps1
#
# Each step is recorded once, with its real output and timestamps, into
# docs/media/demo/*.cast, and rendered to docs/media/*.svg. Each step states
# the exit code it expects; the first step that returns anything else stops the
# script. Nothing is retried: a demo re-run until it looks good would be a
# claim about a rate that was never measured.
#
# Alongside the recordings it writes docs/media/demo/measurements.json: the
# host, the build each step ran, wall time and bytes per run, and the Docker
# build cache before and after. The README's cost figures come from that file.
#
#   pwsh -File scripts/demo/first-five-minutes.ps1 -RenderOnly
#
# redraws docs/media/*.svg from the recordings already in docs/media/demo,
# without Docker and without running anything. The witness needs the catch
# run's verdict.json, which lives in the gitignored run corpus; where that run
# is absent, the existing witness.svg is kept and the script says so.

param([string]$Media = 'docs/media', [switch]$RenderOnly)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
Set-Location $root
$demo = Join-Path $Media 'demo'
New-Item -ItemType Directory -Force $demo | Out-Null

$exe = if ($IsWindows) { '.exe' } else { '' }
$thesisRel = "../../bin/thesis$exe"
$fixture = 'testdata/kvfixture'
$runsDir = Join-Path $fixture '.prothesis/runs'
$fault = 'net.partition(role:leader)@3000..7500'

function Invoke-Render($steps, [string]$catchRun) {
    foreach ($s in $steps) {
        node scripts/demo/render-cast-svg.mjs (Join-Path $demo "$($s.step).cast") (Join-Path $Media "$($s.step).svg") --title $s.title | Write-Host
    }
    $verdict = Join-Path $runsDir "$catchRun/verdict.json"
    if (Test-Path $verdict) {
        node scripts/demo/witness-svg.mjs $verdict (Join-Path $Media 'witness.svg') | Write-Host
    } else {
        Write-Warning "$verdict is not on this machine: witness.svg was NOT redrawn"
    }
}

if ($RenderOnly) {
    $m = Get-Content (Join-Path $demo 'measurements.json') -Raw | ConvertFrom-Json
    Invoke-Render $m.steps $m.steps[0].runs[0].run_id
    exit 0
}

if (-not (Test-Path "bin/thesis$exe")) { throw "bin/thesis$exe is missing: build it with scripts/build.ps1 first" }
& docker version --format '{{.Server.Version}}' *> $null
if ($LASTEXITCODE -ne 0) { throw 'the Docker daemon is not reachable' }
$dirty = [bool](git status --porcelain)
if ($dirty) { Write-Warning 'the working tree is dirty: the recordings will name a dirty build' }

function Get-BuildCache {
    $rows = docker system df --format '{{json .}}' | ForEach-Object { $_ | ConvertFrom-Json }
    $bc = $rows | Where-Object { $_.Type -eq 'Build Cache' }
    [ordered]@{ size = $bc.Size; reclaimable = $bc.Reclaimable }
}
function Get-Runs { @(Get-ChildItem $runsDir -Directory -ErrorAction SilentlyContinue | ForEach-Object Name) }

function Measure-Run([string]$runId) {
    $dir = Join-Path $runsDir $runId
    $vp = Join-Path $dir 'verdict.json'
    $v = if (Test-Path $vp) { Get-Content $vp -Raw | ConvertFrom-Json } else { $null }
    $worlds = foreach ($w in Get-ChildItem $dir -Directory -Filter 'world-*' | Sort-Object Name) {
        $res = Join-Path $w.FullName 'result.json'
        $wt = Join-Path $w.FullName 'world.thesis'
        $phases = [ordered]@{}
        if (Test-Path $wt) {
            foreach ($p in (Get-Content $wt -Raw | ConvertFrom-Json).phase_timings) {
                if ($p.end_ms -ge $p.start_ms) { $phases[$p.phase] = $p.end_ms - $p.start_ms }
            }
        }
        [ordered]@{
            world    = $w.Name
            outcome  = if (Test-Path $res) { (Get-Content $res -Raw | ConvertFrom-Json).outcome } else { $null }
            phase_ms = $phases
            bytes    = (Get-ChildItem $w.FullName -Recurse -File | Measure-Object Length -Sum).Sum
        }
    }
    [ordered]@{
        run_id  = $runId
        verdict = if ($v) { $v.verdict } else { $null }
        commit  = if ($v) { $v.commit } else { $null }
        used_s  = if ($v) { $v.budget.used_s } else { $null }
        worlds  = @($worlds)
        bytes   = (Get-ChildItem $dir -Recurse -File | Measure-Object Length -Sum).Sum
        oracle_lock = if ($v) { $v.oracle_lock.status } else { $null }
    }
}

function Invoke-DemoStep([string]$name, [string]$title, [int]$expect, [object[]]$steps) {
    $spec = [ordered]@{ title = $title; cols = 100; rows = 24; steps = $steps }
    $specPath = Join-Path $demo "$name.steps.json"
    $spec | ConvertTo-Json -Depth 8 | Set-Content $specPath -Encoding utf8
    $before = Get-Runs
    $cast = Join-Path $demo "$name.cast"
    node scripts/demo/record.mjs $specPath $cast | Write-Host
    [IO.File]::Delete((Resolve-Path $specPath))
    $header = (Get-Content $cast -TotalCount 1) | ConvertFrom-Json
    $last = @($header.prothesis.steps | Where-Object { $null -ne $_.exit_code })[-1]
    $new = @(Get-Runs | Where-Object { $before -notcontains $_ })
    $result = [ordered]@{
        step = $name; title = $title; expected_exit = $expect; exit = $last.exit_code
        seconds = $last.seconds; runs = @($new | ForEach-Object { Measure-Run $_ })
    }
    if ($last.exit_code -ne $expect) {
        $script:measure.steps += $result
        $script:measure | ConvertTo-Json -Depth 10 | Set-Content (Join-Path $demo 'measurements.json') -Encoding utf8
        throw "$name returned exit $($last.exit_code), expected $expect; stopping without retrying"
    }
    return $result
}

function Get-RunArgs([string[]]$extra) { @($thesisRel, 'run', '--profile', 'linear') + $extra + @('--fault', $fault) }

$cpu = (Get-CimInstance Win32_Processor -ErrorAction SilentlyContinue | Select-Object -First 1)
$script:measure = [ordered]@{
    schema      = 'prothesis.demo_measurements/v1'
    recorded_at = (Get-Date).ToString('o')
    host        = [ordered]@{
        os         = [Environment]::OSVersion.VersionString
        cpu        = if ($cpu) { $cpu.Name.Trim() } else { $null }
        logical_cpus = [Environment]::ProcessorCount
        docker_server = (docker version --format '{{.Server.Version}}')
        docker_ncpu   = (docker info --format '{{.NCPU}}')
        docker_mem_bytes = [int64](docker info --format '{{.MemTotal}}')
    }
    build       = [ordered]@{ thesis = (& "bin/thesis$exe" version | Select-Object -Last 1); tree_dirty = $dirty }
    build_cache_before = Get-BuildCache
    steps       = @()
}

# 1. Catch the planted defect. One world, seed 1: a FAIL is never downgraded by
#    narrowing, only a PASS is (D-059).
$script:measure.steps += Invoke-DemoStep '01-catch' 'catch the planted stale read' 1 @(
    @{ show = "thesis run --profile linear --worlds 1 --seed 1 --fault '$fault'"
       argv = Get-RunArgs @('--worlds', '1', '--seed', '1'); cwd = $fixture }
)
$catchRun = $script:measure.steps[-1].runs[0].run_id

# 2. The same fixture with the defect patched, over the whole locked profile.
$script:measure.steps += Invoke-DemoStep '02-patched' 'patch the defect and prove the fix' 0 @(
    @{ show = "KV_VARIANT=kvfixed thesis run --profile linear --fault '$fault'"
       argv = Get-RunArgs @(); cwd = $fixture; env = @{ KV_VARIANT = 'kvfixed' } }
)

# 3. Weaken the test: point the driver at the tiny smoke workload. The file's
#    exact bytes are put back afterwards, not HEAD's: restoring from git would
#    silently discard any uncommitted edit someone had made to it.
$yaml = Join-Path $root "$fixture/prothesis.yaml"
$yamlBytes = [IO.File]::ReadAllBytes($yaml)
try {
    $script:measure.steps += Invoke-DemoStep '03-cheat' 'try to weaken the test' 4 @(
        @{ show = "sed -i 's/--profile {profile}/--profile smoke/' prothesis.yaml"
           edit = @{ file = "$fixture/prothesis.yaml"; from = '--profile {profile}'; to = '--profile smoke' } },
        @{ show = "thesis run --profile linear --fault '$fault'"; argv = Get-RunArgs @(); cwd = $fixture }
    )
} finally {
    [IO.File]::WriteAllBytes($yaml, $yamlBytes)
}

# 4. Test less than the locked profile: a PASS is refused.
$script:measure.steps += Invoke-DemoStep '04-narrowed' 'test less, prove less' 2 @(
    @{ show = "KV_VARIANT=kvfixed thesis run --profile linear --worlds 1 --seed 1 --fault '$fault'"
       argv = Get-RunArgs @('--worlds', '1', '--seed', '1'); cwd = $fixture; env = @{ KV_VARIANT = 'kvfixed' } }
)

# 5. Replay the world that failed in step 1, three times.
$world = ".prothesis/runs/$catchRun/world-0001/world.thesis"
$script:measure.steps += Invoke-DemoStep '05-replay' 'replay the failure' 1 @(
    @{ show = "thesis replay $world -k 3 --expect linearizable.kv"
       argv = @($thesisRel, 'replay', $world, '-k', '3', '--expect', 'linearizable.kv'); cwd = $fixture }
)

$script:measure.build_cache_after = Get-BuildCache
$script:measure | ConvertTo-Json -Depth 10 | Set-Content (Join-Path $demo 'measurements.json') -Encoding utf8

Invoke-Render $script:measure.steps $catchRun
Write-Host "done: $($script:measure.steps.Count) steps recorded; catch run $catchRun"
