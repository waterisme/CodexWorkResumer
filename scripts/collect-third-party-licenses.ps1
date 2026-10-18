[CmdletBinding()]
param([switch]$Check)

$ErrorActionPreference = 'Stop'
if ($PSVersionTable.PSVersion.Major -lt 7) {
    throw 'License collection requires PowerShell 7 or later.'
}

$root = Split-Path -Parent $PSScriptRoot
$goMod = Join-Path $root 'scripts\source\go.mod'
$uiRoot = Join-Path $root 'scripts\source\ui'
$lockPath = Join-Path $uiRoot 'package-lock.json'
$outputPath = Join-Path $root 'THIRD_PARTY_LICENSES.txt'

function Get-LicenseText {
    param([string]$Directory, [string]$Description)
    if (-not (Test-Path -LiteralPath $Directory -PathType Container)) {
        throw "Dependency directory missing: $Description"
    }
    $files = @(Get-ChildItem -LiteralPath $Directory -File |
        Where-Object { $_.Name -match '^(LICENSE|LICENCE|COPYING)(\.|$)' } |
        Sort-Object Name)
    if ($files.Count -ne 1) { throw "Expected one license file for $Description; found $($files.Count)." }
    return ([IO.File]::ReadAllText($files[0].FullName, [Text.Encoding]::UTF8) -replace "`r`n?", "`n").TrimEnd("`n")
}

$sections = New-Object 'System.Collections.Generic.List[string]'
[void]$sections.Add('Third-party license texts for CodexWorkResumer')
[void]$sections.Add('Generated from scripts/source/go.mod and scripts/source/ui/package-lock.json.')
[void]$sections.Add('Includes the Go runtime, linked Go modules, and npm non-dev packages.')

$goRoot = (go env GOROOT).Trim()
if ($LASTEXITCODE -ne 0 -or -not $goRoot) { throw 'Could not locate Go toolchain.' }
$goLicense = Get-LicenseText -Directory $goRoot -Description 'Go standard library/runtime'
$patentsPath = Join-Path $goRoot 'PATENTS'
if (-not (Test-Path -LiteralPath $patentsPath -PathType Leaf)) { throw 'Go runtime PATENTS file is missing.' }
$goPatents = ([IO.File]::ReadAllText($patentsPath, [Text.Encoding]::UTF8) -replace "`r`n?", "`n").TrimEnd("`n")
[void]$sections.Add("`n===== Go standard library and runtime: LICENSE =====`n$goLicense")
[void]$sections.Add("`n===== Go standard library and runtime: PATENTS =====`n$goPatents")

$goCache = Join-Path (go env GOPATH) 'pkg\mod'
if ($LASTEXITCODE -ne 0) { throw 'Could not locate Go module cache.' }
$goDependencies = @(Get-Content -LiteralPath $goMod -Encoding UTF8 |
    ForEach-Object {
        if ($_ -match '^\s*(?:require\s+)?([^\s()]+)\s+(v\d+\.\d+\.\d+[^\s]*)') {
            [pscustomobject]@{ Name = $matches[1]; Version = $matches[2] }
        }
    } | Sort-Object Name)
if ($goDependencies.Count -eq 0) { throw 'No Go dependencies found.' }
foreach ($dependency in $goDependencies) {
    $moduleDirectory = Join-Path $goCache (($dependency.Name -replace '/', '\') + '@' + $dependency.Version)
    $license = Get-LicenseText -Directory $moduleDirectory -Description ($dependency.Name + '@' + $dependency.Version)
    [void]$sections.Add("`n===== Go: $($dependency.Name)@$($dependency.Version) =====`n$license")
}

$lock = Get-Content -LiteralPath $lockPath -Raw -Encoding UTF8 | ConvertFrom-Json -AsHashtable
$npmDependencies = @($lock.packages.GetEnumerator() |
    Where-Object { $_.Key -ne '' -and $_.Value.dev -ne $true } |
    Sort-Object Key)
if ($npmDependencies.Count -eq 0) { throw 'No npm production dependencies found.' }
foreach ($dependency in $npmDependencies) {
    $packageDirectory = Join-Path $uiRoot ($dependency.Key -replace '/', '\')
    $license = Get-LicenseText -Directory $packageDirectory -Description $dependency.Key
    $name = $dependency.Key -replace '^node_modules/', ''
    [void]$sections.Add("`n===== npm: $name@$($dependency.Value.version) =====`n$license")
}

$expected = (($sections -join "`n") + "`n")
if ($Check) {
    if (-not (Test-Path -LiteralPath $outputPath -PathType Leaf)) { throw 'THIRD_PARTY_LICENSES.txt is missing.' }
    $actual = [IO.File]::ReadAllText($outputPath, [Text.Encoding]::UTF8)
    if (-not [string]::Equals($actual, $expected, [StringComparison]::Ordinal)) {
        throw 'THIRD_PARTY_LICENSES.txt is out of date; regenerate it from locked dependencies.'
    }
    Write-Host "License bundle verified: Go runtime, $($goDependencies.Count) Go modules, and $($npmDependencies.Count) npm packages."
} else {
    [IO.File]::WriteAllText($outputPath, $expected, [Text.UTF8Encoding]::new($false))
    Write-Host "License bundle generated: Go runtime, $($goDependencies.Count) Go modules, and $($npmDependencies.Count) npm packages."
}
