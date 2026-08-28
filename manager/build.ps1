param(
    [string]$Version = "dev",
    [string]$Commit = "unknown",
    [string]$BuiltAt = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ"),
    [string]$Output = ".\dist\RcdManager.exe"
)

$ErrorActionPreference = "Stop"
$moduleRoot = $PSScriptRoot
$webRoot = Join-Path $moduleRoot "web"
$outputPath = [System.IO.Path]::GetFullPath((Join-Path $moduleRoot $Output))
$outputDirectory = Split-Path -Parent $outputPath

Push-Location $webRoot
try {
    npm ci
    if ($LASTEXITCODE -ne 0) { throw "npm ci failed with exit code $LASTEXITCODE" }
    npm run build
    if ($LASTEXITCODE -ne 0) { throw "npm run build failed with exit code $LASTEXITCODE" }
} finally {
    Pop-Location
}

New-Item -ItemType Directory -Force -Path $outputDirectory | Out-Null
$ldflags = "-s -w -X rcd_manager/internal/buildinfo.Version=$Version -X rcd_manager/internal/buildinfo.Commit=$Commit -X rcd_manager/internal/buildinfo.BuiltAt=$BuiltAt"
Push-Location $moduleRoot
try {
    go test ./...
    if ($LASTEXITCODE -ne 0) { throw "go test failed with exit code $LASTEXITCODE" }
    go build -trimpath -ldflags $ldflags -o $outputPath ./cmd/rcd-manager
    if ($LASTEXITCODE -ne 0) { throw "go build failed with exit code $LASTEXITCODE" }
} finally {
    Pop-Location
}

Write-Host "Built $outputPath"
