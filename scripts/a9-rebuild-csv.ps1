# Rebuild A.9 benchmark rows from the run bundles on disk.
#
# The CSV is a convenience; the RUN BUNDLE is the evidence. Rebuilding from
# verdict.json and search.json rather than from a transcript means the numbers in
# the report are the numbers the tool wrote, and it makes the CSV reconstructible
# after any interruption.
#
# Each entry below names the arm, the seed and the run id that trial produced.
# The run id is what ties a row to its artifacts, and it is what the benchmark's
# uniqueness check is written against.
#
#   usage: pwsh scripts/a9-rebuild-csv.ps1 -Out <csv>

param(
    [string]$Repo    = (Split-Path -Parent $PSScriptRoot),
    [string]$Out     = "$env:TEMP\a9_benchmark.csv",
    [string]$Mapping = "$env:TEMP\a9_mapping.csv"
)

$Fixture = Join-Path $Repo "testdata\kvfixture"
$runs    = Join-Path $Fixture ".prothesis\runs"

if (-not (Test-Path $Mapping)) { throw "no arm/seed -> run id mapping at $Mapping" }

"arm,seed,verdict,worlds,seconds,found_consistency,first_violation_ordinal,first_violation_faults,first_violation_seconds,run_id" |
    Out-File -FilePath $Out -Encoding utf8

$seen = @{}
foreach ($row in (Import-Csv $Mapping)) {
    $dir = Join-Path $runs $row.run_id
    if (-not (Test-Path "$dir\verdict.json")) { throw "$($row.run_id): no verdict.json" }
    if ($seen.ContainsKey($row.run_id)) { throw "$($row.run_id) appears twice in the mapping" }
    $seen[$row.run_id] = "$($row.arm) seed $($row.seed)"

    $v = Get-Content "$dir\verdict.json" -Raw | ConvertFrom-Json
    $found = 0
    if ($v.violations | Where-Object { $_.oracle -eq "linearizable.kv" }) { $found = 1 }

    $worlds = 0; $ord = 0; $nf = 0; $secs = ""
    if (Test-Path "$dir\search.json") {
        $rec = Get-Content "$dir\search.json" -Raw | ConvertFrom-Json
        $worlds = $rec.worlds_run; $ord = $rec.first_violation_ordinal
        $nf = $rec.first_violation_fault_count
        if ($null -ne $rec.first_violation_seconds) { $secs = $rec.first_violation_seconds }
    }

    "{0},{1},{2},{3},{4},{5},{6},{7},{8},{9}" -f `
        $row.arm, $row.seed, $v.verdict, $worlds, $row.seconds, $found, $ord, $nf, $secs, $row.run_id |
        Out-File -FilePath $Out -Encoding utf8 -Append
}

Write-Host "rebuilt $($seen.Count) rows into $Out"
Import-Csv $Out | Format-Table -AutoSize
