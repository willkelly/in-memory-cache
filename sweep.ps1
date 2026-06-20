<#
.SYNOPSIS
  Publication sweep for Track A. Runs the benchmarks in three phases and merges
  them into one benchstat input, special-casing the copy-on-write write path.

.DESCRIPTION
  cow's write path is O(keys): at 1M keys each Set copies the whole map
  (~50-120 ms/op), and Go's benchmark ramp-up overshoots wildly on such slow
  ops. So cow's write-heavy cells are measured with a small FIXED iteration
  count (-benchtime=<n>x) -- a handful of samples already pin a 100 ms op --
  while everything else (including cow's lock-free reads) gets precise
  time-based measurement.

  Phases (all at the same -keys/-count/-cpu):
    A. mutex, rwmutex, syncmap, sharded  -- all mixes, -benchtime time-based
    B. cow, read-only (mix=r100)         -- -benchtime time-based
    C. cow, write mixes (r90/r50/r10)    -- -benchtime fixed iterations

.EXAMPLE
  .\sweep.ps1 -Keys 1000000 -Count 10 -Cpu "1,2,4,8"
#>
param(
  [int]    $Keys          = 1000000,
  [int]    $KeyLen        = 16,
  [int]    $Count         = 10,
  [string] $Cpu           = "1,2,4,8",
  [string] $Benchtime     = "1s",
  [string] $CowWriteIters = "20x",
  [string] $OutDir        = "results"
)

$ErrorActionPreference = "Stop"
$env:Path = [Environment]::GetEnvironmentVariable("Path", "Machine") + ";" +
            [Environment]::GetEnvironmentVariable("Path", "User")

$go = (Get-Command go -ErrorAction SilentlyContinue).Source
if (-not $go) { throw "go not found on PATH" }
$benchstat = (Join-Path ((& $go env GOPATH).Trim()) "bin\benchstat.exe")
if (-not (Test-Path $benchstat)) { throw "benchstat not found: $benchstat" }

$cpuList = ($Cpu.Trim() -replace '\s+', ',')
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
$raw = Join-Path $OutDir "bench.txt"

function Invoke-Bench($pattern, $bt, $label) {
  Write-Host "`n### phase: $label  (-bench '$pattern' -benchtime=$bt)"
  $a = @(
    "test", "-bench", $pattern, "-benchmem",
    "-count=$Count", "-cpu=$cpuList",
    "-keys=$Keys", "-keylen=$KeyLen", "-benchtime=$bt",
    "-run", "^$"
  )
  & $go @a | Tee-Object -Variable lines
  return $lines
}

# go test -bench splits the pattern on unbracketed '/', matching each element
# against the corresponding path segment (BenchmarkCache / impl=.. / dist=.. / mix=..).
# NOTE: anchor mix patterns with $ -- "r10" is a substring of "r100", so an
# unanchored mix=(...|r10) would also match the read-only r100 cells.
$phaseA = Invoke-Bench 'BenchmarkCache/impl=(mutex|rwmutex|syncmap|sharded)' $Benchtime    'fast impls'
$phaseB = Invoke-Bench 'BenchmarkCache/impl=cow/dist=(uniform|zipf)/mix=r100$' $Benchtime   'cow reads'
$phaseC = Invoke-Bench 'BenchmarkCache/impl=cow/dist=(uniform|zipf)/mix=(r10|r50|r90)$' $CowWriteIters 'cow writes'

# Merge: keep phase A in full (its header lines orient benchstat), then append
# only the Benchmark result lines from B and C.
$merged  = @()
$merged += $phaseA
$merged += ($phaseB | Where-Object { $_ -match '^Benchmark' })
$merged += ($phaseC | Where-Object { $_ -match '^Benchmark' })
$merged | Out-File -FilePath $raw -Encoding utf8

$summary = Join-Path $OutDir "summary.txt"
Write-Host "`n=== summary (mean +/- CV) -> $summary ==="
& $benchstat $raw | Tee-Object -Variable s
$s | Out-File -FilePath $summary -Encoding utf8

$byImpl = Join-Path $OutDir "by-impl.txt"
Write-Host "`n=== comparison (impl as columns) -> $byImpl ==="
& $benchstat -col /impl $raw | Tee-Object -Variable c
$c | Out-File -FilePath $byImpl -Encoding utf8

Write-Host "`nDone. Wrote: $raw, $summary, $byImpl"
