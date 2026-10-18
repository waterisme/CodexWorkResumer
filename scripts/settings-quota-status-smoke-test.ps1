[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$settingsPath = Join-Path $PSScriptRoot 'source\ui\settings.ps1'
$tokens = $null
$parseErrors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseFile($settingsPath, [ref]$tokens, [ref]$parseErrors)
if ($parseErrors.Count -gt 0) { throw "settings.ps1 has parse errors: $($parseErrors -join '; ')" }

$definition = $ast.Find({
    param($node)
    $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and
        $node.Name -eq 'Get-ActiveQuotaWaitCount'
}, $false)
if (-not $definition) { throw 'Get-ActiveQuotaWaitCount is missing.' }
. ([ScriptBlock]::Create($definition.Extent.Text))

$cases = @(
    @{ Name = 'no dashboard'; Dashboard = $null; Expected = 0 },
    @{ Name = 'historical catalog failure'; Dashboard = [pscustomobject]@{ tasks = @(
        [pscustomobject]@{ status = 'rate_limited'; wait_reason = '' },
        [pscustomobject]@{ status = 'stopped'; wait_reason = 'quota_limited' }
    ) }; Expected = 0 },
    @{ Name = 'ordinary pending'; Dashboard = [pscustomobject]@{ tasks = @(
        [pscustomobject]@{ status = 'pending'; wait_reason = 'retry_backoff' }
    ) }; Expected = 0 },
    @{ Name = 'active quota waits'; Dashboard = [pscustomobject]@{ tasks = @(
        [pscustomobject]@{ status = 'rate_limited'; wait_reason = '' },
        [pscustomobject]@{ status = 'pending'; wait_reason = 'quota_limited' },
        [pscustomobject]@{ status = 'pending'; wait_reason = 'quota_status_unavailable' }
    ) }; Expected = 2 }
)
foreach ($case in $cases) {
    $actual = Get-ActiveQuotaWaitCount $case.Dashboard
    if ($actual -ne $case.Expected) {
        throw "$($case.Name): expected $($case.Expected) active quota waits, got $actual."
    }
}

[pscustomobject]@{ Status = 'passed'; Cases = $cases.Count }
