[CmdletBinding()]
param(
    [string]$OutputDirectory = (Join-Path $env:USERPROFILE 'releases\CodexWorkResumer'),
    [switch]$SkipBuild
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2

$pluginRoot = Split-Path -Parent $PSScriptRoot
$verifiedVersion = & (Join-Path $PSScriptRoot 'verify-version.ps1') -Root $pluginRoot
$releaseTemplate = Join-Path $pluginRoot 'release\windows'
. (Join-Path $releaseTemplate 'common.ps1')
$installLauncher = ([string][char]0x5b89) + ([char]0x88dd) + '.cmd'
$uninstallLauncher = ([string][char]0x5378) + ([char]0x8f09) + '.cmd'
$installReadme = 'README-' + ([char]0x5b89) + ([char]0x88dd) + ([char]0x8aaa) + ([char]0x660e) + '.txt'
$startupManagerLauncher = ([string][char]0x555f) + ([char]0x52d5) + ([char]0x7ba1) + ([char]0x7406) + ([char]0x5668) + '.cmd'
$safeDisableLauncher = ([string][char]0x5b89) + ([char]0x5168) + ([char]0x505c) + ([char]0x7528) + '.cmd'
$startupManagerVbs = 'startup-manager.vbs'
$safeCodexLauncher = ([string][char]0x5b89) + ([char]0x5168) + ([char]0x555f) + ([char]0x52d5) + 'Codex.vbs'

$pluginManifestPath = Join-Path $pluginRoot '.codex-plugin\plugin.json'
$pluginManifest = Read-JsonDocument -Path $pluginManifestPath
if ($null -eq $pluginManifest -or [string]$pluginManifest.name -ne 'codex-auto-retry') {
    throw 'The Codex Auto Retry plugin manifest is missing or invalid.'
}
$pluginVersion = [string]$pluginManifest.version
$versionRecord = Read-JsonDocument -Path (Join-Path $pluginRoot 'VERSION.json')
if ($null -eq $versionRecord -or [int]$versionRecord.schema -ne 1 -or
    [string]$versionRecord.version -notmatch '^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$' -or
    [int]$versionRecord.build -lt 1) {
    throw 'VERSION.json is missing or invalid.'
}
$packageVersion = [string]$versionRecord.version
$buildNumber = [int]$versionRecord.build
if ($packageVersion -notmatch '^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$') {
    throw "The plugin version is not suitable for a release package: $pluginVersion"
}
if ($pluginVersion -ne ($packageVersion + '+build.' + $buildNumber)) {
    throw 'The plugin version must match VERSION.json as version+build.N.'
}

$versionSource = Get-Content -LiteralPath (Join-Path $pluginRoot 'scripts\source\model.go') -Raw -Encoding UTF8
if ($versionSource -notmatch 'appVersion\s*=\s*"([^"]+)"' -or $matches[1] -ne $packageVersion -or
    $versionSource -notmatch 'appBuild\s*=\s*(\d+)' -or [int]$matches[1] -ne $buildNumber) {
    throw 'The watchdog appVersion and plugin base version do not match.'
}

foreach ($required in @('common.ps1', 'deploy.ps1', 'uninstall-release.ps1', 'startup-manager.ps1', $startupManagerVbs, $safeCodexLauncher, $installLauncher, $uninstallLauncher, $startupManagerLauncher, $safeDisableLauncher, $installReadme)) {
    if (-not (Test-Path -LiteralPath (Join-Path $releaseTemplate $required) -PathType Leaf)) {
        throw "Release template file is missing: $required"
    }
}

if (-not $SkipBuild) {
    Write-Host '[Release] Building and testing application binaries...'
    & (Join-Path $PSScriptRoot 'build.ps1') | Out-Host
}

$watchdog = Join-Path $pluginRoot 'scripts\bin\codex-auto-retry.exe'
. (Join-Path $PSScriptRoot 'build-provenance.ps1')
Assert-CodexBuildProvenance -Root $pluginRoot
$mcp = Join-Path $pluginRoot 'scripts\bin\codex-auto-retry-mcp.exe'
Assert-X64PeBinary -Path $watchdog
Assert-X64PeBinary -Path $mcp

$outputPath = Get-FullPath $OutputDirectory
New-Item -ItemType Directory -Force -Path $outputPath | Out-Null
$packageName = "CodexWorkResumer-v$packageVersion-b$buildNumber-windows-x64"
$archivePath = Join-Path $outputPath ($packageName + '.zip')
$archiveHashPath = $archivePath + '.sha256.txt'
$stageParent = Join-Path ([System.IO.Path]::GetTempPath()) ('codex-auto-retry-release-' + [guid]::NewGuid().ToString('N'))
$packageRoot = Join-Path $stageParent $packageName

try {
    New-Item -ItemType Directory -Force -Path $packageRoot | Out-Null

    Write-Host '[Release] Staging one-click installer...'
    foreach ($file in @('common.ps1', 'deploy.ps1', 'uninstall-release.ps1', 'startup-manager.ps1', $startupManagerVbs, $safeCodexLauncher, $installLauncher, $uninstallLauncher, $startupManagerLauncher, $safeDisableLauncher, $installReadme)) {
        Copy-Item -LiteralPath (Join-Path $releaseTemplate $file) -Destination (Join-Path $packageRoot $file) -Force
    }

    $payloadRoot = Join-Path $packageRoot 'payload\codex-auto-retry'
    New-Item -ItemType Directory -Force -Path $payloadRoot | Out-Null
    # Installation does not need documentation screenshots or editable documents.
    # Keep these out of the installer even when old local copies remain on disk.
    foreach ($entry in @('.codex-plugin', '.gitattributes', '.gitignore', '.mcp.json', 'release', 'skills', 'scripts', 'LICENSE', 'README.md', 'README_zh-TW.md', 'THIRD_PARTY_NOTICES.md', 'THIRD_PARTY_LICENSES.txt', 'VERSION.json', 'CHANGELOG.md')) {
        $source = Join-Path $pluginRoot $entry
        if (-not (Test-Path -LiteralPath $source)) { throw "Plugin payload entry is missing: $entry" }
        if ($entry -ne 'scripts') {
            Copy-Item -LiteralPath $source -Destination (Join-Path $payloadRoot $entry) -Recurse -Force
            continue
        }
        # npm ci creates very deep paths on Windows. Never stage node_modules:
        # copying it first can hit MAX_PATH before the cleanup below runs.
        $stagedScripts = Join-Path $payloadRoot 'scripts'
        $stagedSource = Join-Path $stagedScripts 'source'
        $stagedUi = Join-Path $stagedSource 'ui'
        New-Item -ItemType Directory -Force -Path $stagedUi | Out-Null
        foreach ($item in (Get-ChildItem -LiteralPath $source -Force | Where-Object Name -ne 'source')) {
            Copy-Item -LiteralPath $item.FullName -Destination (Join-Path $stagedScripts $item.Name) -Recurse -Force
        }
        $sourceTree = Join-Path $source 'source'
        foreach ($item in (Get-ChildItem -LiteralPath $sourceTree -Force | Where-Object { $_.Name -ne 'ui' -and $_.Name -ne '.go-test-tmp' })) {
            Copy-Item -LiteralPath $item.FullName -Destination (Join-Path $stagedSource $item.Name) -Recurse -Force
        }
        $uiTree = Join-Path $sourceTree 'ui'
        foreach ($item in (Get-ChildItem -LiteralPath $uiTree -Force | Where-Object Name -ne 'node_modules')) {
            Copy-Item -LiteralPath $item.FullName -Destination (Join-Path $stagedUi $item.Name) -Recurse -Force
        }
    }

    $nodeModules = Join-Path $payloadRoot 'scripts\source\ui\node_modules'
    if (Test-Path -LiteralPath $nodeModules -PathType Container) {
        $nodeModulesItem = Get-Item -LiteralPath $nodeModules -Force
        if (($nodeModulesItem.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
            throw 'The staged node_modules path is a link and cannot be safely removed.'
        }
        Remove-Item -LiteralPath $nodeModules -Recurse -Force
    }

    $stagedBin = Join-Path $payloadRoot 'scripts\bin'
    $allowedBinFiles = @('codex-auto-retry.exe', 'codex-auto-retry-mcp.exe')
    $unexpectedBinEntries = @(Get-ChildItem -LiteralPath $stagedBin -Force |
        Where-Object { $allowedBinFiles -notcontains $_.Name })
    foreach ($entry in $unexpectedBinEntries) {
        Remove-Item -LiteralPath $entry.FullName -Recurse -Force
    }

    $releaseManifest = [pscustomobject][ordered]@{
        schemaVersion = 1
        product = 'CodexWorkResumer'
        packageVersion = $packageVersion
        buildNumber = $buildNumber
        pluginName = 'codex-auto-retry'
        pluginVersion = $pluginVersion
        target = 'windows-x64'
        payloadPath = 'payload/codex-auto-retry'
        installer = $installLauncher
        uninstaller = $uninstallLauncher
        createdAt = [DateTime]::UtcNow.ToString('o')
        requirements = @(
            'Windows 10 or 11 x64',
            'Codex App installed and started at least once',
            'Current-user write access; administrator rights are not required'
        )
    }
    Write-JsonAtomic -Path (Join-Path $packageRoot 'release-manifest.json') -Value $releaseManifest

    $sumLines = New-Object System.Collections.Generic.List[string]
    Get-ChildItem -LiteralPath $packageRoot -File -Recurse -Force |
        Where-Object { $_.Name -ne 'SHA256SUMS.txt' } |
        Sort-Object FullName |
        ForEach-Object {
            $relative = Get-RelativePackagePath -BasePath $packageRoot -Path $_.FullName
            $hash = (Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash.ToLowerInvariant()
            [void]$sumLines.Add($hash + '  ' + $relative)
        }
    Write-Utf8NoBom -Path (Join-Path $packageRoot 'SHA256SUMS.txt') -Content (($sumLines -join [Environment]::NewLine) + [Environment]::NewLine)

    Remove-Item -LiteralPath $archivePath -Force -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $archiveHashPath -Force -ErrorAction SilentlyContinue
    Write-Host '[Release] Compressing archive...'
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    [System.IO.Compression.ZipFile]::CreateFromDirectory(
        $stageParent,
        $archivePath,
        [System.IO.Compression.CompressionLevel]::Optimal,
        $false
    )

    $archiveHash = (Get-FileHash -LiteralPath $archivePath -Algorithm SHA256).Hash.ToLowerInvariant()
    [System.IO.File]::WriteAllText(
        $archiveHashPath,
        ($archiveHash + '  ' + (Split-Path -Leaf $archivePath) + [Environment]::NewLine),
        [System.Text.Encoding]::ASCII
    )

    $archive = Get-Item -LiteralPath $archivePath
    [pscustomobject]@{
        Package = $archive.FullName
        Bytes = $archive.Length
        SHA256 = $archiveHash
        HashFile = $archiveHashPath
        PluginVersion = $pluginVersion
        Target = 'Windows x64'
    }
}
finally {
    if (Test-Path -LiteralPath $stageParent -PathType Container) {
        $tempRoot = (Get-FullPath ([System.IO.Path]::GetTempPath())).TrimEnd('\') + '\'
        $stageFull = Get-FullPath $stageParent
        if ($stageFull.StartsWith($tempRoot, [System.StringComparison]::OrdinalIgnoreCase)) {
            Remove-Item -LiteralPath $stageFull -Recurse -Force -ErrorAction SilentlyContinue
        }
    }
}
