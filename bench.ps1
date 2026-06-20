<#
.SYNOPSIS
  Run the in-process (Track A) cache benchmarks with repetition and summarize
  them with benchstat, producing confidence-interval and comparison tables.

.EXAMPLE
  # Headline sweep across core counts:
  .\bench.ps1 -Keys 1000000 -Count 10 -Cpu 1,2,4,8

.EXAMPLE
  # Quick local check:
  .\bench.ps1 -Keys 5000 -Count 6 -Cpu 4 -Benchtime 100ms
#>
param(
  [int]    $Keys      = 100000,
  [int]    $KeyLen    = 16,
  [int]    $Count     = 10,
  [string] $Cpu       = "1,2,4,8",
  [string] $Benchtime = "1s",
  [string] $Bench     = "BenchmarkCache",
  [string] $OutDir    = "results"
)

$ErrorActionPreference = "Stop"

# winget added Go/benchstat to the persistent PATH, but a shell started before
# the install won't see them. Refresh PATH from the registry for this process.
$env:Path = [Environment]::GetEnvironmentVariable("Path", "Machine") + ";" +
            [Environment]::GetEnvironmentVariable("Path", "User")

$go = (Get-Command go -ErrorAction SilentlyContinue).Source
if (-not $go) { throw "go not found on PATH" }
$gobin     = (& $go env GOPATH).Trim() + "\bin"
$benchstat = Join-Path $gobin "benchstat.exe"
if (-not (Test-Path $benchstat)) {
  throw "benchstat not found at $benchstat (go install golang.org/x/perf/cmd/benchstat@latest)"
}

New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
$raw = Join-Path $OutDir "bench.txt"

Write-Host "go test -bench $Bench -count=$Count -cpu=$Cpu -keys=$Keys -keylen=$KeyLen -benchtime=$Benchtime"
Write-Host "raw results -> $raw`n"

# Tee to the host for live progress AND capture for a UTF-8 file. (PowerShell
# 5.1's Tee-Object -FilePath writes UTF-16, which benchstat cannot parse, so we
# capture to a variable and Out-File -Encoding utf8 instead.)
# No 2>&1: go test writes results to stdout; redirecting stderr in PS 5.1 would
# wrap lines as errors and falsely flip $?.
# Normalize $Cpu: an unquoted "-Cpu 1,2,4,8" is parsed as the array {1,2,4,8}
# and coerced to this [string] param as "1 2 4 8" (space-joined); go test wants
# commas. Collapse any whitespace to commas, accepting either input form.
$cpuList = ($Cpu.Trim() -replace '\s+', ',')

$testArgs = @(
  "test", "-bench", $Bench, "-benchmem",
  "-count=$Count", "-cpu=$cpuList",
  "-keys=$Keys", "-keylen=$KeyLen", "-benchtime=$Benchtime",
  "-run", "^$"
)
& $go @testArgs | Tee-Object -Variable rawLines
$rawLines | Out-File -FilePath $raw -Encoding utf8

# 1. Per-benchmark mean +/- variation (the confidence-interval view).
$summary = Join-Path $OutDir "summary.txt"
Write-Host "`n=== summary (mean +/- CV) -> $summary ==="
& $benchstat $raw | Tee-Object -Variable sumLines
$sumLines | Out-File -FilePath $summary -Encoding utf8

# 2. Implementations pivoted into columns; rows are the remaining axes
#    (value size, distribution, mix) plus the GOMAXPROCS configuration.
$byImpl = Join-Path $OutDir "by-impl.txt"
Write-Host "`n=== comparison (impl as columns) -> $byImpl ==="
& $benchstat -col /impl $raw | Tee-Object -Variable colLines
$colLines | Out-File -FilePath $byImpl -Encoding utf8

Write-Host "`nDone. Wrote: $raw, $summary, $byImpl"
