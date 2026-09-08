param([string]$Version = '0.2.0')
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
try {
    & npm ci --prefix web
    if ($LASTEXITCODE -ne 0) { throw 'npm ci failed' }
    & npm run build --prefix web
    if ($LASTEXITCODE -ne 0) { throw 'Frontend build failed' }
    & go test ./...
    if ($LASTEXITCODE -ne 0) { throw 'Tests failed' }
    $packHelper = Join-Path $env:TEMP ('pikpak-vault-package-' + [guid]::NewGuid().ToString('N') + '.exe')
    & go build -o $packHelper ./scripts/package
    if ($LASTEXITCODE -ne 0) { throw 'Package helper build failed' }
    $savedGOOS = $env:GOOS
    $savedGOARCH = $env:GOARCH
    $savedCGO = $env:CGO_ENABLED
    try {
        $env:CGO_ENABLED = '0'
        foreach ($arch in @('amd64','arm64')) {
            $bundle = Join-Path $root "bin/pikpak-vault-$Version-linux-$arch"
            New-Item -ItemType Directory -Path $bundle -Force | Out-Null
            $env:GOOS = 'linux'
            $env:GOARCH = $arch
            & go build -trimpath -ldflags "-s -w -X pikpakvault/internal/vault.Version=$Version" -o (Join-Path $bundle 'vault') ./cmd/vault
            if ($LASTEXITCODE -ne 0) { throw "Build failed: $arch" }
            Copy-Item -LiteralPath deploy -Destination $bundle -Recurse -Force
            Copy-Item -LiteralPath docs -Destination $bundle -Recurse -Force
            Copy-Item -LiteralPath README.md,THIRD_PARTY.md -Destination $bundle -Force
            $archive = "pikpak-vault-$Version-linux-$arch.tar.gz"
            & $packHelper $bundle (Join-Path $root "bin/$archive")
            if ($LASTEXITCODE -ne 0) { throw 'Packaging failed' }
        }
    } finally {
        $env:GOOS = $savedGOOS
        $env:GOARCH = $savedGOARCH
        $env:CGO_ENABLED = $savedCGO
        Remove-Item -LiteralPath $packHelper -ErrorAction SilentlyContinue
    }
    $sums = Get-ChildItem (Join-Path $root 'bin') -Filter "pikpak-vault-$Version-linux-*.tar.gz" | Sort-Object Name | ForEach-Object { (Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash.ToLower() + '  ' + $_.Name }
    [IO.File]::WriteAllText((Join-Path $root 'bin/SHA256SUMS'), ($sums -join "`n") + "`n", [Text.UTF8Encoding]::new($false))
} finally { Pop-Location }
