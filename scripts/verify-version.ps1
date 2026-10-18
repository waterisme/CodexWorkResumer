[CmdletBinding()]
param(
    [string]$Root
)

$ErrorActionPreference = 'Stop'
if (-not $Root) { $Root = Split-Path -Parent $PSScriptRoot }
$versionPath = Join-Path $Root 'VERSION.json'
$version = Get-Content -LiteralPath $versionPath -Raw -Encoding UTF8 | ConvertFrom-Json
if ([int]$version.schema -ne 1 -or [string]$version.version -notmatch '^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$' -or [int]$version.build -lt 1) {
    throw 'VERSION.json is missing or invalid.'
}
$publicVersion = [string]$version.version
$buildNumber = [int]$version.build
$pluginVersion = $publicVersion + '+build.' + $buildNumber

$model = Get-Content -LiteralPath (Join-Path $Root 'scripts\source\model.go') -Raw -Encoding UTF8
if ($model -notmatch 'appVersion\s*=\s*"([^"]+)"' -or $matches[1] -ne $publicVersion) {
    throw 'scripts/source/model.go appVersion does not match VERSION.json.'
}
if ($model -notmatch 'appBuild\s*=\s*(\d+)' -or [int]$matches[1] -ne $buildNumber) {
    throw 'scripts/source/model.go appBuild does not match VERSION.json.'
}

$plugin = Get-Content -LiteralPath (Join-Path $Root '.codex-plugin\plugin.json') -Raw -Encoding UTF8 | ConvertFrom-Json
if ([string]$plugin.version -ne $pluginVersion) {
    throw ".codex-plugin/plugin.json must use $pluginVersion."
}

foreach ($relative in @('scripts\source\ui\package.json', 'scripts\source\ui\package-lock.json')) {
    $packageText = Get-Content -LiteralPath (Join-Path $Root $relative) -Raw -Encoding UTF8
    if ($packageText -notmatch '"version"\s*:\s*"([^"]+)"' -or $matches[1] -ne $publicVersion) {
        throw "$relative does not match VERSION.json."
    }
}

$panel = Get-Content -LiteralPath (Join-Path $Root 'scripts\source\ui\src\panel.ts') -Raw -Encoding UTF8
if (($panel | Select-String -Pattern ('version: "' + [regex]::Escape($publicVersion) + '"') -AllMatches).Matches.Count -lt 2) {
    throw 'The embedded panel runtime and preview versions do not match VERSION.json.'
}
if ($panel -notmatch 'build:\s*(\d+)' -or [int]$matches[1] -ne $buildNumber) {
    throw 'The embedded panel preview build does not match VERSION.json.'
}

$changelog = Get-Content -LiteralPath (Join-Path $Root 'CHANGELOG.md') -Raw -Encoding UTF8
if ($changelog -notmatch ('(?m)^##\s+' + [regex]::Escape($publicVersion + '+' + $buildNumber) + '\b')) {
    throw 'CHANGELOG.md is missing the current version and build entry.'
}

[pscustomobject]@{
    Version = $publicVersion
    Build = $buildNumber
    Display = "v$publicVersion (Build $buildNumber)"
    PluginVersion = $pluginVersion
}
