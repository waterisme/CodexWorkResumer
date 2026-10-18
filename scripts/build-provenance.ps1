function Get-CodexBuildSourceHash {
    param([string]$Root)
    $source = Join-Path $Root 'scripts\source'
    [string[]]$filePaths = @(Get-ChildItem -LiteralPath $source -File -Recurse | Where-Object {
        $_.FullName -notmatch '[\\/]node_modules[\\/]' -and (
            $_.Extension -eq '.go' -or $_.Name -in @('go.mod','go.sum','panel.html','settings.ps1'))
    } | ForEach-Object { $_.FullName })
    if ($filePaths.Count -eq 0) { throw 'Build sources are missing.' }
    [Array]::Sort($filePaths, [StringComparer]::Ordinal)
    $lines = foreach ($filePath in $filePaths) {
        $filePath.Substring($source.Length).Replace('\','/') + ':' + (Get-FileHash -LiteralPath $filePath -Algorithm SHA256).Hash
    }
    foreach ($rootFile in @('VERSION.json', 'CHANGELOG.md', '.codex-plugin\plugin.json')) {
        $path = Join-Path $Root $rootFile
        if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { throw "Build metadata is missing: $rootFile" }
        $lines += $rootFile.Replace('\','/') + ':' + (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash
    }
    $sha = [Security.Cryptography.SHA256]::Create()
    try { return ([BitConverter]::ToString($sha.ComputeHash([Text.Encoding]::UTF8.GetBytes(($lines -join "`n"))))).Replace('-','') }
    finally { $sha.Dispose() }
}

function Write-CodexBuildProvenance {
    param([string]$Root)
    $version = Get-Content -LiteralPath (Join-Path $Root 'VERSION.json') -Raw | ConvertFrom-Json
    $record = [ordered]@{
        schema = 2
        version = [string]$version.version
        build = [int]$version.build
        source_hash = Get-CodexBuildSourceHash $Root
    }
    foreach ($name in @('codex-auto-retry.exe','codex-auto-retry-mcp.exe')) {
        $record[$name] = (Get-FileHash -LiteralPath (Join-Path $Root ('scripts\bin\' + $name))).Hash
    }
    [IO.File]::WriteAllText((Join-Path $Root 'scripts\build-info.json'), ($record | ConvertTo-Json), [Text.UTF8Encoding]::new($false))
}

function Assert-CodexBuildProvenance {
    param([string]$Root)
    $path = Join-Path $Root 'scripts\build-info.json'
    if (-not (Test-Path -LiteralPath $path)) { throw 'Build provenance missing. Run scripts/build.ps1; do not package stale executables.' }
    $record = Get-Content -LiteralPath $path -Raw | ConvertFrom-Json
    $version = Get-Content -LiteralPath (Join-Path $Root 'VERSION.json') -Raw | ConvertFrom-Json
    if ($record.schema -ne 2 -or $record.version -ne [string]$version.version -or
        [int]$record.build -ne [int]$version.build -or
        $record.source_hash -ne (Get-CodexBuildSourceHash $Root)) {
        throw 'Sources or version metadata changed since the last build. Rebuild before packaging.'
    }
    foreach ($name in @('codex-auto-retry.exe','codex-auto-retry-mcp.exe')) {
        if ($record.$name -ne (Get-FileHash -LiteralPath (Join-Path $Root ('scripts\bin\' + $name))).Hash) { throw "Build binary mismatch: $name" }
    }
}
