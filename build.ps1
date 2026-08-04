$ErrorActionPreference = "Stop"
$output = Join-Path $PSScriptRoot "dist\windows\amd64"
New-Item -ItemType Directory -Force -Path $output | Out-Null
Push-Location $PSScriptRoot
try {
    go test ./...
    if ($LASTEXITCODE -ne 0) { throw "go test failed with exit code $LASTEXITCODE" }
    go build -buildmode=c-shared -o (Join-Path $output "usage-quota-stats.dll") .
    if ($LASTEXITCODE -ne 0) { throw "go build failed with exit code $LASTEXITCODE. Install a C compiler and set CGO_ENABLED=1." }
    Remove-Item -Force -ErrorAction SilentlyContinue (Join-Path $output "usage-quota-stats.h")
} finally {
    Pop-Location
}
