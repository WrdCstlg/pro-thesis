# Mutation check for the Phase 4 search engine.
#
# Every assertion this phase makes should FAIL when the thing it asserts is
# broken. A green suite proves nothing on its own; a green suite that goes red
# under each of these edits is evidence the tests can fail.
#
# usage: pwsh scripts/mutate-phase4.ps1

$ErrorActionPreference = "Stop"
$repo = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
Set-Location $repo

$mutations = @(
  @{ name = "UCT: restore A.5's literal (U_avg / visits) form"
     file = "internal\search\saboteur\uct.go"
     from = "return avgUtility + c*math.Sqrt(math.Log(float64(parentVisits))/float64(visits))"
     to   = "return avgUtility/float64(visits) + c*math.Sqrt(math.Log(float64(parentVisits))/float64(visits))"
     pkg  = "./internal/search/saboteur/" }

  @{ name = "UCT: make an unvisited child finite, so a good visited sibling can beat it"
     file = "internal\search\saboteur\uct.go"
     from = "	if visits <= 0 {`n		return math.Inf(1)`n	}"
     to   = "	if visits <= 0 {`n		return 1e9`n	}"
     pkg  = "./internal/search/saboteur/" }

  @{ name = "UCT: drop the parsimony tie-break"
     file = "internal\search\saboteur\uct.go"
     from = "	if aFaults != bFaults {`n		return aFaults < bFaults`n	}"
     to   = "	if false {`n		return aFaults < bFaults`n	}"
     pkg  = "./internal/search/saboteur/" }

  @{ name = "MCTS: drop A.7 Rung 4's overlap bias from the expansion order"
     file = "internal\search\saboteur\mcts.go"
     from = "			if overlaps(p, spec) {`n				return PriorityOverlap`n			}"
     to   = "			if overlaps(p, spec) {`n				return PriorityLadder`n			}"
     pkg  = "./internal/search/saboteur/" }

  @{ name = "MCTS: ignore the probe's measured target as a prior"
     file = "internal\search\saboteur\mcts.go"
     from = "	if priorTarget != `"`" && a.Target == priorTarget {`n		return PriorityPrior`n	}"
     to   = "	if false && a.Target == priorTarget {`n		return PriorityPrior`n	}"
     pkg  = "./internal/search/saboteur/" }

  @{ name = "MCTS: drop the pending marker, so a 4-wide batch runs one world four times"
     file = "internal\search\saboteur\mcts.go"
     from = "		if c.Pending && c.Visits == 0 {`n			continue`n		}"
     to   = "		if false {`n			continue`n		}"
     pkg  = "./internal/search/saboteur/" }

  @{ name = "MCTS: let an UNEVALUABLE rollout prune a branch"
     file = "internal\search\saboteur\mcts.go"
     from = "	if !res.Evaluable {`n		return`n	}"
     to   = "	if false {`n		return`n	}"
     pkg  = "./internal/search/saboteur/" }

  @{ name = "MCTS: let an added fault open AFTER what is already committed"
     file = "internal\search\saboteur\mcts.go"
     from = "	at := earliest - t.opts.Timing.CompoundLeadInMS"
     to   = "	at := earliest + t.opts.Timing.CompoundLeadInMS"
     pkg  = "./internal/search/saboteur/" }

  @{ name = "PROBE: drop A.3's no-fault control world"
     file = "internal\search\saboteur\probe.go"
     from = "	plan.Probes = append(plan.Probes, Probe{Index: 0, Control: true, Label: ProbeControlLabel})"
     to   = "	_ = ProbeControlLabel"
     pkg  = "./internal/search/saboteur/" }

  @{ name = "PROBE: rank an UNEVALUABLE probe on the numbers it happens to carry"
     file = "internal\search\saboteur\probe.go"
     from = "		if a.Evaluable != b.Evaluable {`n			return a.Evaluable`n		}"
     to   = "		if false {`n			return a.Evaluable`n		}"
     pkg  = "./internal/search/saboteur/" }

  @{ name = "PROBE: escalate a DAMPEN signal"
     file = "internal\search\saboteur\probe.go"
     from = "		if !s.Evaluable || !s.Class.Reinforcing() {`n			continue`n		}"
     to   = "		if false {`n			continue`n		}"
     pkg  = "./internal/search/saboteur/" }

  @{ name = "PROBE: use the mean rather than the median for a baseline"
     file = "internal\search\saboteur\probe.go"
     from = "	sort.Float64s(vals)`n	mid := len(vals) / 2"
     to   = "	mid := len(vals) / 2"
     pkg  = "./internal/search/saboteur/" }

  @{ name = "ESCALATE: make consistency prefix-closed, so a DRIVE finding stops the search"
     file = "internal\search\saboteur\escalate.go"
     from = "var PrefixClosedClasses = [...]schema.OracleClass{schema.ClassCrash, schema.ClassSafety}"
     to   = "var PrefixClosedClasses = [...]schema.OracleClass{schema.ClassCrash, schema.ClassSafety, schema.ClassConsistency}"
     pkg  = "./internal/search/saboteur/" }

  @{ name = "ESCALATE: let an indefinite finding terminate the search"
     file = "internal\search\saboteur\escalate.go"
     from = "	if !in.Definite || !in.Violated {"
     to   = "	if false {"
     pkg  = "./internal/search/saboteur/" }

  @{ name = "ESCALATE: root Tier 2 at the PROBE's minimal magnitude"
     file = "internal\search\saboteur\escalate.go"
     from = "	case schema.FaultProcPause:`n		dur = opts.Timing.PauseMS"
     to   = "	case schema.FaultProcPause:`n		dur = DefaultProbeWindowMS"
     pkg  = "./internal/search/saboteur/" }

  @{ name = "OBSERVE: ignore the measured sampling rate and assume A.3's 200ms"
     file = "internal\search\saboteur\strategy.go"
     from = "	if o.SampleIntervalMS > 0 {`n		opt.SampleIntervalMS = int(o.SampleIntervalMS)`n	}"
     to   = "	if false {`n		opt.SampleIntervalMS = int(o.SampleIntervalMS)`n	}"
     pkg  = "./internal/search/saboteur/" }

  @{ name = "ENGINE: run a search on a profile that does not enable it"
     file = "internal\search\engine\engine.go"
     from = "	if !prof.Search {"
     to   = "	if false {"
     pkg  = "./internal/search/engine/" }

  @{ name = "ENGINE: restrict only one arm's action space"
     file = "internal\search\engine\engine.go"
     from = "			delete(sp.Targets, k)`n			continue"
     to   = "			continue"
     pkg  = "./internal/search/engine/" }

  @{ name = "ENGINE: treat a harness-error world as observed evidence"
     file = "internal\search\engine\engine.go"
     from = "	out.Observed = hasDrive(o.Phases) && o.Outcome != control.OutcomeHarnessError"
     to   = "	out.Observed = true"
     pkg  = "./internal/search/engine/" }

  @{ name = "ENGINE: accept an undefaulted search block and invent the weights"
     file = "internal\search\engine\engine.go"
     from = "	if cfg.Search.MaxMCTSDepth < 1 {"
     to   = "	if false {"
     pkg  = "./internal/search/engine/" }

  @{ name = "CONTROL: pass the RUN profile where the DRIVER profile belongs"
     file = "internal\control\runner.go"
     from = "	plan, err := driver.NewPlan(r.cfg, r.driverProfile, paths.History, worldSeed)"
     to   = "	plan, err := driver.NewPlan(r.cfg, r.profile, paths.History, worldSeed)"
     pkg  = "./internal/control/" }

  @{ name = "CONTROL: hardcode PlannedFaults empty again"
     file = "internal\control\runner.go"
     from = "		PlannedFaults:     plannedFaultWindows(req.Realized),"
     to   = "		PlannedFaults:     []oracle.FaultWindow{},"
     pkg  = "./internal/control/" }

  @{ name = "CONTROL: let a fault with no recorded binding excuse every crash"
     file = "internal\control\runner.go"
     from = "			Nodes:   append([]string(nil), rf.Nodes...),"
     to   = "			Nodes:   []string{`"kv-n1`", `"kv-n2`", `"kv-n3`"},"
     pkg  = "./internal/control/" }
)

$caught = 0
$missed = @()

foreach ($m in $mutations) {
    $path = Join-Path $repo $m.file
    $orig = Get-Content $path -Raw
    if (-not $orig.Contains($m.from)) {
        Write-Host ("SKIP (pattern gone) : " + $m.name) -ForegroundColor Yellow
        $missed += ("PATTERN GONE: " + $m.name)
        continue
    }
    Set-Content -NoNewline -Path $path -Value $orig.Replace($m.from, $m.to)
    $out = & go test $m.pkg 2>&1
    $failed = ($LASTEXITCODE -ne 0)
    Set-Content -NoNewline -Path $path -Value $orig

    if ($failed) {
        Write-Host ("CAUGHT : " + $m.name) -ForegroundColor Green
        $caught++
    } else {
        Write-Host ("MISSED : " + $m.name) -ForegroundColor Red
        $missed += $m.name
    }
}

Write-Host ""
Write-Host ("caught {0} of {1} mutations" -f $caught, $mutations.Count)
if ($missed.Count -gt 0) {
    Write-Host "NOT CAUGHT:" -ForegroundColor Red
    $missed | ForEach-Object { Write-Host ("  - " + $_) }
    exit 1
}
