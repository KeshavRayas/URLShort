# E2E runner. Resets the local test stack, then runs the suite against the real
# binary, real Postgres and real Redis.
#
#   ./scripts/e2e.ps1
#
# The suite writes e2e-report.md and e2e-report.json at the repo root.

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

Write-Host "==> resetting postgres + redis"
& cmd /c "docker compose down -v --remove-orphans >nul 2>&1"
& cmd /c "docker compose up -d postgres redis >nul 2>&1"
if ($LASTEXITCODE -ne 0) { throw "docker compose up failed" }

Write-Host "==> waiting for postgres"
$cid = (docker compose ps -q postgres).Trim()
$deadline = (Get-Date).AddSeconds(60)
while ($true) {
    $health = & docker inspect --format '{{.State.Health.Status}}' $cid 2>$null
    if ($health -eq "healthy") { break }
    if ((Get-Date) -gt $deadline) { throw "postgres did not become healthy" }
    Start-Sleep -Milliseconds 500
}

Write-Host "==> running e2e suite"
go test ./e2e -v -count=1
$code = $LASTEXITCODE

Write-Host ""
if (Test-Path "e2e-report.md") {
    Get-Content "e2e-report.md" -Encoding UTF8
}

exit $code
