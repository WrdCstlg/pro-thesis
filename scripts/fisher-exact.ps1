# One-sided Fisher's exact test on a 2x2 table, in exact integer arithmetic.
#
# D-029 pre-registers this as the Phase 4 statistical test, because A.9 #4's
# "median time-to-first-violation" is undefined on right-censored data: a trial
# that never finds the bug has no finite time. The count of trials that found one
# within a fixed budget is the same claim made testable.
#
#   usage: pwsh scripts/fisher-exact.ps1 -Found1 9 -Miss1 1 -Found2 2 -Miss2 8
#
#            found   not found
#   arm 1    Found1    Miss1
#   arm 2    Found2    Miss2
#
# Reports the one-sided p for "arm 1 > arm 2": the probability, under the null of
# no difference and with both margins fixed, of a table at least this extreme in
# that direction.
#
#     P(a) = C(r1, a) * C(r2, k - a) / C(n, k)
#
# with r1, r2 the row totals and k the number of successes. Summed for a >= the
# observed Found1. BigInteger throughout, so the arithmetic is exact and the
# result does not depend on how a float happened to round.
#
# The parameter names are spelled out because `-A` and `-C` collide with
# PowerShell's own common-parameter abbreviations and silently bind to the wrong
# values -- which is exactly the kind of thing that turns a benchmark into a
# decoration.

param(
    [Parameter(Mandatory = $true)][int]$Found1,
    [Parameter(Mandatory = $true)][int]$Miss1,
    [Parameter(Mandatory = $true)][int]$Found2,
    [Parameter(Mandatory = $true)][int]$Miss2
)

function Fact([int]$n) {
    $r = [System.Numerics.BigInteger]::One
    for ($i = 2; $i -le $n; $i++) { $r = $r * [System.Numerics.BigInteger]$i }
    return $r
}

function Choose([int]$n, [int]$k) {
    if ($k -lt 0 -or $k -gt $n) { return [System.Numerics.BigInteger]::Zero }
    return (Fact $n) / ((Fact $k) * (Fact ($n - $k)))
}

$r1 = $Found1 + $Miss1
$r2 = $Found2 + $Miss2
$n  = $r1 + $r2
$k  = $Found1 + $Found2

$total = Choose $n $k
if ($total.IsZero) { throw "degenerate table" }

$lo = [Math]::Max(0, $k - $r2)
$hi = [Math]::Min($r1, $k)

$tail = [System.Numerics.BigInteger]::Zero
for ($a = $Found1; $a -le $hi; $a++) {
    $tail += (Choose $r1 $a) * (Choose $r2 ($k - $a))
}

# Exact rational -> decimal, to nine places.
$scaled = [System.Numerics.BigInteger]::Divide($tail * [System.Numerics.BigInteger]1000000000, $total)
$p = [double]$scaled / 1000000000.0

Write-Host ""
Write-Host "                 found   not found     total"
Write-Host ("  arm 1  {0,10} {1,11} {2,9}" -f $Found1, $Miss1, $r1)
Write-Host ("  arm 2  {0,10} {1,11} {2,9}" -f $Found2, $Miss2, $r2)
Write-Host ("  total  {0,10} {1,11} {2,9}" -f $k, ($Miss1 + $Miss2), $n)
Write-Host ""
Write-Host ("  support: a in [{0}, {1}];  observed a = {2}" -f $lo, $hi, $Found1)
Write-Host ("  one-sided Fisher's exact, arm 1 > arm 2:  p = {0:N6}" -f $p)
Write-Host ("  alpha = 0.05  ->  {0}" -f $(if ($p -lt 0.05) { "SIGNIFICANT" } else { "NOT significant" }))
Write-Host ""
