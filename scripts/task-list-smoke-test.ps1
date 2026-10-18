[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$settingsScript = Join-Path $PSScriptRoot 'source\ui\settings.ps1'
$binary = Join-Path $PSScriptRoot 'bin\codex-auto-retry.exe'
$testRoot = Join-Path $env:TEMP ('codex-auto-retry-list-' + [guid]::NewGuid().ToString('N'))
$settingsProcess = $null

try {
    if (-not (Test-Path -LiteralPath $binary -PathType Leaf)) {
        throw "Built watchdog not found: $binary"
    }
    New-Item -ItemType Directory -Force -Path $testRoot | Out-Null
    $config = [ordered]@{
        config_version = 4
        retry_prompt = 'Continue isolated Session list test.'
        max_recovery_attempts = 100
        max_consecutive_retries = 10
        memory_limit_mb = 4096
        resume_interval_seconds = 60
        usage_refresh_minutes = 1
        delay_strategy = 'fixed'
        initial_delay_seconds = 2
        max_delay_seconds = 30
        delay_increment_seconds = 1
        auto_scan_enabled = $true
        scan_interval_seconds = 60
        quota_resume_mode = 'staggered_start'
        max_parallel_sessions = 2
        shared_app_server_port = 0
        show_notifications = $false
    }
    [System.IO.File]::WriteAllText(
        (Join-Path $testRoot 'config.json'),
        (ConvertTo-Json -InputObject $config -Depth 4),
        [System.Text.UTF8Encoding]::new($false)
    )
    $tasks = @(
        for ($index = 0; $index -lt 40; $index++) {
            [ordered]@{
                thread_id = ('10000000-0000-4000-8000-{0:D12}' -f $index)
                label = ('官方測試工作 {0:D2}' -f $index)
                project_path = 'C:\SessionSmoke'
                task_type = 'Codex'
                status = 'completed'
                last_activity_at = [DateTimeOffset]::UtcNow.AddMinutes(-$index).ToString('o')
            }
        }
    )
    $dashboard = [ordered]@{ tasks = $tasks; automatic_recovery_supported = $false }
    [System.IO.File]::WriteAllText(
        (Join-Path $testRoot 'dashboard.json'),
        (ConvertTo-Json -InputObject $dashboard -Depth 6),
        [System.Text.UTF8Encoding]::new($false)
    )
    [System.IO.File]::WriteAllText(
        (Join-Path $testRoot 'settings-list-fixture.flag'),
        '1',
        [System.Text.UTF8Encoding]::new($false)
    )
    $launchArguments = '-NoProfile -STA -ExecutionPolicy Bypass -File "{0}" -DataDir "{1}" -Executable "{2}" -SmokeTest' -f
        $settingsScript, $testRoot, $binary
    $stdoutPath = Join-Path $testRoot 'settings-stdout.txt'
    $stderrPath = Join-Path $testRoot 'settings-stderr.txt'
    $settingsProcess = Start-Process -FilePath (Get-Command powershell.exe).Source `
        -ArgumentList $launchArguments -WindowStyle Hidden -PassThru `
        -RedirectStandardOutput $stdoutPath -RedirectStandardError $stderrPath
    if (-not $settingsProcess.WaitForExit(15000)) {
        $stderrText = if (Test-Path -LiteralPath $stderrPath) { Get-Content -Raw -LiteralPath $stderrPath } else { '' }
        throw "The Session list settings smoke test did not exit. $stderrText"
    }
    $errorPath = Join-Path $testRoot 'settings-smoke-error.txt'
    if (Test-Path -LiteralPath $errorPath) {
        throw ('The settings window reported an error: ' + (Get-Content -Raw -Encoding UTF8 -LiteralPath $errorPath))
    }
    $marker = Join-Path $testRoot 'settings-list-smoke.ok'
    if (-not (Test-Path -LiteralPath $marker) -or
        (Get-Content -Raw -Encoding UTF8 -LiteralPath $marker).Trim() -ne 'scroll-and-selection-preserved') {
        throw 'The Session list did not preserve its scroll, selection, checkbox and title.'
    }
    [pscustomobject]@{
        Status = 'passed'
        SessionRows = $tasks.Count
        ScrollPreserved = $true
        SelectionPreserved = $true
        CheckboxPreserved = $true
        OfficialTitlePreserved = $true
    }
}
finally {
    if ($settingsProcess -and -not $settingsProcess.HasExited) {
        Stop-Process -Id $settingsProcess.Id -Force -ErrorAction SilentlyContinue
    }
    if (Test-Path -LiteralPath $testRoot) {
        $resolvedTestRoot = [System.IO.Path]::GetFullPath($testRoot)
        $resolvedTemp = [System.IO.Path]::GetFullPath($env:TEMP).TrimEnd('\') + '\'
        if (-not $resolvedTestRoot.StartsWith($resolvedTemp, [System.StringComparison]::OrdinalIgnoreCase)) {
            throw "Refusing to remove unexpected smoke-test path: $resolvedTestRoot"
        }
        Remove-Item -LiteralPath $resolvedTestRoot -Recurse -Force
    }
}
