$ErrorActionPreference = "Stop"

$ProjectDir = $PSScriptRoot
$Output = Join-Path $ProjectDir "pocketbase-vector.exe"

Write-Host "Building pocketbase-vector..." -ForegroundColor Cyan

$env:CGO_ENABLED  = "1"
$env:GOOS         = "windows"
$env:GOARCH       = "amd64"
$env:CGO_CFLAGS   = "-I$ProjectDir"

go build -ldflags="-s -w" -o $Output .

if ($LASTEXITCODE -ne 0) {
    Write-Host "Build failed." -ForegroundColor Red
    exit $LASTEXITCODE
}

$size = (Get-Item $Output).Length / 1MB
Write-Host ("Build succeeded: {0} ({1:F1} MB)" -f $Output, $size) -ForegroundColor Green
