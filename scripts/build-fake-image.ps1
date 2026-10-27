param(
    [ValidateSet('amd64', 'arm64')]
    [string]$Architecture = 'amd64'
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
$previousLocation = Get-Location
$savedGoos = $env:GOOS
$savedGoarch = $env:GOARCH
$savedCgo = $env:CGO_ENABLED
try {
    Set-Location -LiteralPath $repoRoot
    New-Item -ItemType Directory -Force -Path 'dist/docker' | Out-Null
    $env:GOOS = 'linux'
    $env:GOARCH = $Architecture
    $env:CGO_ENABLED = '0'
    go build -buildvcs=false -trimpath -o dist/docker/agentbench-worker ./cmd/agentbench-worker
    if ($LASTEXITCODE -ne 0) { throw 'Fake worker build failed.' }
    docker build --network none --pull=false --platform "linux/$Architecture" --file docker/fake.Dockerfile --tag agentbench-fake:local .
    if ($LASTEXITCODE -ne 0) { throw 'Fake image build failed.' }
    docker image inspect agentbench-fake:local --format '{{.Id}}'
    if ($LASTEXITCODE -ne 0) { throw 'Fake image inspection failed.' }
}
finally {
    $env:GOOS = $savedGoos
    $env:GOARCH = $savedGoarch
    $env:CGO_ENABLED = $savedCgo
    Set-Location -LiteralPath $previousLocation
}
