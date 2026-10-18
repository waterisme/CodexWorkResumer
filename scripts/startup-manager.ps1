[CmdletBinding()]
param(
    [ValidateSet('gui', 'status', 'enable', 'disable', 'start', 'stop', 'launch-codex', 'safe-disable', 'uninstall')]
    [string]$Action = 'gui',
    [switch]$RemoveData,
    [switch]$NoPrompt,
    [string]$UserProfileRoot = $env:USERPROFILE,
    [string]$LocalAppDataRoot = $env:LOCALAPPDATA,
    [string]$RunName = 'CodexAutoRetry',
    [string]$ReleaseRoot = ''
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2
if ($env:OS -ne 'Windows_NT') { throw 'The startup manager supports Windows only.' }

$profileRoot = [System.IO.Path]::GetFullPath($UserProfileRoot)
$localAppDataRoot = [System.IO.Path]::GetFullPath($LocalAppDataRoot)
$installDir = Join-Path $localAppDataRoot 'CodexAutoRetry'
$watchdog = Join-Path $installDir 'codex-auto-retry.exe'
$pluginTarget = Join-Path $profileRoot 'plugins\codex-auto-retry'
$statusPath = Join-Path $installDir 'status.json'
$configPath = Join-Path $installDir 'config.json'
$sharedStatePath = Join-Path $installDir 'shared-server.json'
$runSubKey = 'Software\Microsoft\Windows\CurrentVersion\Run'
. (Join-Path $PSScriptRoot 'startup-approval.ps1')
. (Join-Path $PSScriptRoot 'shared-server-status.ps1')

$managerLanguagePath = Join-Path $installDir 'ui-language.json'
$managerI18nPath = Join-Path $PSScriptRoot 'startup-manager-i18n.json'
$script:managerLanguage = 'zh'
$script:managerI18n = Get-Content -Raw -Encoding UTF8 -LiteralPath $managerI18nPath | ConvertFrom-Json
if (Test-Path -LiteralPath $managerLanguagePath -PathType Leaf) {
    try {
        $savedManagerLanguage = Get-Content -Raw -Encoding UTF8 -LiteralPath $managerLanguagePath | ConvertFrom-Json
        if ([string]$savedManagerLanguage.language -eq 'en') { $script:managerLanguage = 'en' }
    }
    catch { }
}

function Get-ManagerText {
    param([Parameter(Mandatory = $true)][string]$Key)
    $languageTable = $script:managerI18n.PSObject.Properties[$script:managerLanguage].Value
    $property = $languageTable.PSObject.Properties[$Key]
    if ($null -ne $property) { return [string]$property.Value }
    return $Key
}

function Save-ManagerLanguage {
    New-Item -ItemType Directory -Force -Path $installDir | Out-Null
    $record = [ordered]@{ language = $script:managerLanguage }
    $temporary = $managerLanguagePath + '.tmp-' + [guid]::NewGuid().ToString('N')
    try {
        [System.IO.File]::WriteAllText($temporary, ($record | ConvertTo-Json), [System.Text.UTF8Encoding]::new($false))
        Move-Item -LiteralPath $temporary -Destination $managerLanguagePath -Force
    }
    finally {
        Remove-Item -LiteralPath $temporary -Force -ErrorAction SilentlyContinue
    }
}

function Switch-ManagerLanguage {
    $script:managerLanguage = if ($script:managerLanguage -eq 'zh') { 'en' } else { 'zh' }
    Save-ManagerLanguage
}

function Open-RunKey {
    param([bool]$Writable)
    return [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey($runSubKey, $Writable)
}

function Get-RunValue {
    $key = Open-RunKey -Writable $false
    if ($null -eq $key) { return '' }
    try {
        $value = $key.GetValue($RunName, $null, [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
        if ($null -eq $value) { return '' }
        return [string]$value
    }
    finally {
        $key.Close()
    }
}

function Test-OwnedStartupValue {
    param([AllowNull()][string]$Value)
    if ([string]::IsNullOrWhiteSpace($Value)) { return $false }
    $trimmed = $Value.Trim()
    if ($trimmed.StartsWith('"')) {
        $closingQuote = $trimmed.IndexOf('"', 1)
        if ($closingQuote -le 1) { return $false }
        $executable = $trimmed.Substring(1, $closingQuote - 1)
    }
    else {
        $executable = ($trimmed -split '[\s\t]', 2)[0]
    }
    return [string]::Equals($executable, $watchdog, [System.StringComparison]::OrdinalIgnoreCase)
}

function Restore-ManagedStartupValue {
    param([AllowNull()][string]$Value)
    $key = Open-RunKey -Writable $true
    if ($null -eq $key) {
        if ([string]::IsNullOrWhiteSpace($Value)) { return }
        throw 'The current-user startup registry key could not be opened while restoring the previous value.'
    }
    try {
        if ([string]::IsNullOrWhiteSpace($Value)) {
            $key.DeleteValue($RunName, $false)
        }
        else {
            $key.SetValue($RunName, $Value, [Microsoft.Win32.RegistryValueKind]::String)
        }
    }
    finally {
        $key.Close()
    }
}

function Set-ManagedStartup {
    if (-not (Test-Path -LiteralPath $watchdog -PathType Leaf)) {
        throw "The watchdog executable is missing: $watchdog"
    }
    $existing = Get-RunValue
    if (-not [string]::IsNullOrWhiteSpace($existing) -and -not (Test-OwnedStartupValue $existing)) {
        throw "The startup entry $RunName belongs to another command and was not changed."
    }
    $oldApproval = Get-CodexAutoRetryStartupApproval -RunName $RunName
    $desiredValue = ('"{0}" supervise' -f $watchdog)
    [byte[]]$expectedApprovalBytes = @(Get-CodexAutoRetryStartupApprovalEnabledBytes -ExistingBytes $(if ($oldApproval.Present) { [byte[]]$oldApproval.Bytes } else { $null }))
    try {
        $key = Open-RunKey -Writable $true
        if ($null -eq $key) {
            $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey($runSubKey, $true)
        }
        if ($null -eq $key) { throw 'The current-user startup registry key could not be opened.' }
        try {
            $key.SetValue($RunName, $desiredValue, [Microsoft.Win32.RegistryValueKind]::String)
        }
        finally {
            $key.Close()
        }
        $null = Set-CodexAutoRetryStartupApprovalEnabled -RunName $RunName
        $actual = Get-RunValue
        if (-not (Test-OwnedStartupValue $actual) -or $actual -notmatch '(?i)\bsupervise\b') {
            throw 'The startup entry could not be registered in supervised mode.'
        }
    }
    catch {
        try {
            # Restore only while the registry still contains the value this
            # operation wrote. A concurrent user or installer change is left
            # untouched and reported through the original failure.
            if ((Get-RunValue) -eq $desiredValue) {
                $currentApproval = Get-CodexAutoRetryStartupApproval -RunName $RunName
                $currentApprovalBytes = if ($currentApproval.Present) { [byte[]]$currentApproval.Bytes } else { $null }
                $approvalWasOld = Test-CodexAutoRetryStartupApprovalBytes -Left $currentApprovalBytes -Right $(if ($oldApproval.Present) { [byte[]]$oldApproval.Bytes } else { $null })
                $approvalWasWritten = Test-CodexAutoRetryStartupApprovalBytes -Left $currentApprovalBytes -Right $expectedApprovalBytes
                if ($approvalWasOld -or $approvalWasWritten) {
                    Restore-ManagedStartupValue -Value $existing
                    if ($approvalWasWritten) {
                        if ($oldApproval.Present) {
                            Restore-CodexAutoRetryStartupApproval -RunName $RunName -Bytes ([byte[]]$oldApproval.Bytes)
                        }
                        else {
                            $null = Remove-CodexAutoRetryStartupApproval -RunName $RunName
                        }
                    }
                }
            }
        }
        catch {
            # Preserve the original failure. The next status/repair pass can
            # report the remaining registry inconsistency without masking it.
        }
        throw
    }
    return $actual
}

function Remove-ManagedStartup {
    $existing = Get-RunValue
    if (-not [string]::IsNullOrWhiteSpace($existing) -and -not (Test-OwnedStartupValue $existing)) {
        throw "The startup entry $RunName belongs to another command and was not removed."
    }
    $oldApproval = Get-CodexAutoRetryStartupApproval -RunName $RunName
    $removedRun = $false
    try {
        # Re-read immediately before deletion so a concurrent installer or
        # user cannot replace the checked owned command with a foreign one.
        $current = Get-RunValue
        if ($current -ne $existing) {
            throw "The startup entry $RunName changed while it was being removed."
        }
        if (-not [string]::IsNullOrWhiteSpace($existing)) {
            $key = Open-RunKey -Writable $true
            if ($null -ne $key) {
                try { $key.DeleteValue($RunName, $false); $removedRun = $true }
                finally { $key.Close() }
            }
        }
        if (-not [string]::IsNullOrWhiteSpace((Get-RunValue))) {
            throw 'The plugin startup entry is still present after removal.'
        }
        $removedApproval = Remove-CodexAutoRetryStartupApproval -RunName $RunName
        return $removedRun -or $removedApproval
    }
    catch {
        try {
            # Roll back only an unchanged post-delete state. Never overwrite a
            # foreign command that appeared while the removal was in flight.
            $currentRunAfterFailure = Get-RunValue
            $currentApproval = Get-CodexAutoRetryStartupApproval -RunName $RunName
            $currentApprovalBytes = if ($currentApproval.Present) { [byte[]]$currentApproval.Bytes } else { $null }
            $approvalWasOld = Test-CodexAutoRetryStartupApprovalBytes -Left $currentApprovalBytes -Right $(if ($oldApproval.Present) { [byte[]]$oldApproval.Bytes } else { $null })
            $approvalWasRemoved = -not $currentApproval.Present
            $runWasRemoved = [string]::IsNullOrWhiteSpace($currentRunAfterFailure)
            $runWasUnchanged = $currentRunAfterFailure -eq $existing
            if (($runWasRemoved -or $runWasUnchanged) -and ($approvalWasOld -or $approvalWasRemoved)) {
                if ($runWasRemoved) {
                    Restore-ManagedStartupValue -Value $existing
                }
                if ($approvalWasRemoved -and $oldApproval.Present) {
                    Restore-CodexAutoRetryStartupApproval -RunName $RunName -Bytes ([byte[]]$oldApproval.Bytes)
                }
            }
        }
        catch {
            # Preserve the original failure and leave status visible for a
            # subsequent explicit repair.
        }
        throw
    }
}

function Get-ManagerProcesses {
    if (-not (Test-Path -LiteralPath $watchdog -PathType Leaf)) { return @() }
    return @(Get-CimInstance Win32_Process -ErrorAction SilentlyContinue |
        Where-Object {
            $_.ExecutablePath -and
            [string]::Equals($_.ExecutablePath, $watchdog, [System.StringComparison]::OrdinalIgnoreCase)
        })
}

function Test-HeartbeatFresh {
    param(
        [Parameter(Mandatory = $true)]$Value,
        [TimeSpan]$MaxAge = ([TimeSpan]::FromSeconds(15))
    )
    try {
        $timestamp = if ($Value -is [DateTime]) {
            [DateTimeOffset]$Value
        }
        elseif ($Value -is [DateTimeOffset]) {
            $Value
        }
        else {
            [DateTimeOffset]::Parse([string]$Value)
        }
        $age = [DateTimeOffset]::UtcNow - $timestamp.ToUniversalTime()
        return $age -ge [TimeSpan]::Zero -and $age -le $MaxAge
    }
    catch {
        return $false
    }
}

function Read-JsonOrNull {
    param([Parameter(Mandatory = $true)][string]$Path)
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { return $null }
    try { return (Get-Content -Raw -Encoding UTF8 -LiteralPath $Path | ConvertFrom-Json) }
    catch { return $null }
}

function Get-ManagerState {
    $status = Read-JsonOrNull -Path $statusPath
    $config = Read-JsonOrNull -Path $configPath
    $sharedState = Read-JsonOrNull -Path $sharedStatePath
    $statusReadFailed = (Test-Path -LiteralPath $statusPath -PathType Leaf) -and $null -eq $status
    $sharedStateReadFailed = (Test-Path -LiteralPath $sharedStatePath -PathType Leaf) -and $null -eq $sharedState
    $processes = @(Get-ManagerProcesses)
    $heartbeatFresh = $false
    $heartbeatValue = Get-CodexAutoRetryStatusProperty -Status $status -Name 'last_heartbeat_at' -Default (Get-CodexAutoRetryStatusProperty -Status $status -Name 'last_scan_at')
    if ($status -and $heartbeatValue) {
        $heartbeatFresh = Test-HeartbeatFresh -Value $heartbeatValue
    }
    $serviceRunning = $null -ne $status -and [bool]$status.running -and $heartbeatFresh -and $processes.Count -gt 0
    $startupEntry = Get-RunValue
    $startupApproval = Get-CodexAutoRetryStartupApproval -RunName $RunName
    $startupMode = if ([string]::IsNullOrWhiteSpace($startupEntry)) {
        'missing'
    }
    elseif ($startupEntry -match '(?i)\bsupervise\b') {
        'supervise'
    }
    elseif ($startupEntry -match '(?i)\brun\b') {
        'run'
    }
    else {
        'unknown'
    }
    $expectedSharedPort = if ($config -and $config.PSObject.Properties['shared_app_server_port']) { [int]$config.shared_app_server_port } else { 0 }
    $statusCompatibility = Get-CodexAutoRetryStatusCompatibility -Status $status -ReadFailed $statusReadFailed
    $sharedVerification = if ($sharedStateReadFailed) {
        [pscustomobject][ordered]@{ Status = 'unknown'; Reason = 'state_unreadable'; PID = $null; Endpoint = $null }
    } else {
        Get-CodexAutoRetrySharedServerStatus -State $sharedState -ExpectedPort $expectedSharedPort
    }
    $sharedStateStatus = [string]$sharedVerification.Status
    $endpoint = [Environment]::GetEnvironmentVariable('CODEX_APP_SERVER_WS_URL', 'User')
    $manifest = Read-JsonOrNull -Path (Join-Path $pluginTarget '.codex-plugin\plugin.json')
    return [pscustomobject][ordered]@{
        PluginInstalled = $null -ne $manifest -and [string]$manifest.name -eq 'codex-auto-retry'
        PluginVersion = if ($manifest) { [string]$manifest.version } else { $null }
        InstallDir = $installDir
        ServiceRunning = $serviceRunning
        ProcessCount = $processes.Count
        ProcessIds = @($processes | ForEach-Object { [int]$_.ProcessId })
        HeartbeatFresh = $heartbeatFresh
        RuntimeVersion = if ($status) { [string]$status.version } else { $null }
        LastScanAt = if ($status) { $status.last_scan_at } else { $null }
        StartupMode = $startupMode
        StartupEntry = if ([string]::IsNullOrWhiteSpace($startupEntry)) { $null } else { $startupEntry }
        StartupOwned = Test-OwnedStartupValue $startupEntry
        StartupApproved = $startupApproval.Status
        SharedModeEnabled = if ($config -and $config.PSObject.Properties['shared_app_server_enabled']) { [bool]$config.shared_app_server_enabled } else { $false }
        SharedModeRequested = if ($config -and $config.PSObject.Properties['shared_app_server_requested']) { [bool]$config.shared_app_server_requested } elseif ($config -and $config.PSObject.Properties['shared_app_server_enabled']) { [bool]$config.shared_app_server_enabled } else { $false }
        SharedEndpointConfigured = -not [string]::IsNullOrWhiteSpace($endpoint)
        DesktopLaunchMode = Get-CodexAutoRetryStatusProperty -Status $status -Name 'desktop_launch_mode' -Default 'legacy_unprotected'
        SafeLauncher = Join-Path $PSScriptRoot 'launch-codex.ps1'
        SharedServerState = $sharedStateStatus
        SharedServerVerification = [string]$sharedVerification.Reason
        SharedAppServerMemoryUsageMB = Get-CodexAutoRetryStatusProperty -Status $status -Name 'shared_app_server_memory_usage_mb' -Default 0
        SharedAppServerMemoryLimitMB = Get-CodexAutoRetryStatusProperty -Status $status -Name 'shared_app_server_memory_limit_mb' -Default $null
        SharedAppServerMemoryGuardTriggered = [bool](Get-CodexAutoRetryStatusProperty -Status $status -Name 'shared_app_server_memory_guard_triggered' -Default $false)
        RetrySafetyWarning = [string](Get-CodexAutoRetryStatusProperty -Status $status -Name 'retry_safety_warning' -Default '')
        StatusCompatibility = $statusCompatibility
        StatusCompatibilityMessage = Get-CodexAutoRetryStatusCompatibilityMessage -Status $statusCompatibility
        DataDirectoryExists = Test-Path -LiteralPath $installDir -PathType Container
    }
}

function Wait-ManagerServiceState {
    param([Parameter(Mandatory = $true)][bool]$Running, [int]$TimeoutSeconds = 15)
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    do {
        Start-Sleep -Milliseconds 250
        $state = Get-ManagerState
        if ($state.ServiceRunning -eq $Running) { return $state }
    } while ((Get-Date) -lt $deadline)
    return Get-ManagerState
}

function Start-ManagedService {
    $current = Get-ManagerState
    if ($current.ServiceRunning) { return $current }
    if (-not (Test-Path -LiteralPath $watchdog -PathType Leaf)) {
        throw "The watchdog executable is missing: $watchdog"
    }
    New-Item -ItemType Directory -Force -Path $installDir | Out-Null
    Remove-Item -LiteralPath (Join-Path $installDir 'stop.signal') -Force -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath (Join-Path $installDir 'supervisor.stop') -Force -ErrorAction SilentlyContinue
    Start-Process -FilePath $watchdog -ArgumentList @('supervise') -WorkingDirectory $installDir -WindowStyle Hidden | Out-Null
    $state = Wait-ManagerServiceState -Running $true
    if (-not $state.ServiceRunning) { throw 'The watchdog did not publish a fresh heartbeat after starting.' }
    return $state
}

function Stop-ManagedService {
    if (-not (Test-Path -LiteralPath $installDir -PathType Container)) { return Get-ManagerState }
    New-Item -ItemType File -Force -Path (Join-Path $installDir 'supervisor.stop') | Out-Null
    New-Item -ItemType File -Force -Path (Join-Path $installDir 'stop.signal') | Out-Null
    $deadline = (Get-Date).AddSeconds(12)
    do {
        $processes = @(Get-ManagerProcesses)
        if ($processes.Count -eq 0) { break }
        Start-Sleep -Milliseconds 250
    } while ((Get-Date) -lt $deadline)
    $remaining = @(Get-ManagerProcesses)
    if ($remaining.Count -gt 0) {
        throw 'The watchdog did not stop gracefully. No process was force-terminated; use the safe-disable action after Codex closes.'
    }
    return Get-ManagerState
}

function Invoke-ManagedScript {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [string[]]$Arguments = @()
    )
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { throw "Required maintenance script is missing: $Path" }
    $output = (& powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $Path @Arguments 2>&1 | Out-String)
    if ($LASTEXITCODE -ne 0) { throw "Maintenance script failed with exit code $LASTEXITCODE.`n$($output.Trim())" }
    return $output
}

function Invoke-ManagerSafeDisable {
    $safeDisable = Join-Path $pluginTarget 'scripts\safe-disable.ps1'
    if (-not (Test-Path -LiteralPath $safeDisable -PathType Leaf) -and -not [string]::IsNullOrWhiteSpace($ReleaseRoot)) {
        $safeDisable = Join-Path $ReleaseRoot 'payload\codex-auto-retry\scripts\safe-disable.ps1'
    }
    $null = Invoke-ManagedScript -Path $safeDisable -Arguments @('-DataDir', $installDir)
    return Get-ManagerState
}

function Invoke-ManagerUninstall {
    $uninstaller = ''
    if (-not [string]::IsNullOrWhiteSpace($ReleaseRoot)) {
        $uninstaller = Join-Path $ReleaseRoot 'uninstall-release.ps1'
    }
    if (-not (Test-Path -LiteralPath $uninstaller -PathType Leaf)) {
        throw 'For a complete uninstall, run this manager from the extracted release folder so it can remove the Codex plugin registration safely.'
    }
    $arguments = @('-UserProfileRoot', $profileRoot, '-LocalAppDataRoot', $localAppDataRoot)
    if ($RemoveData) { $arguments += '-RemoveData' }
    $null = Invoke-ManagedScript -Path $uninstaller -Arguments $arguments
    return [pscustomobject][ordered]@{
        Uninstalled = $true
        DataRemoved = [bool]$RemoveData
        StartupEntry = Get-RunValue
        RuntimePathExists = Test-Path -LiteralPath $installDir -PathType Container
        PluginPathExists = Test-Path -LiteralPath $pluginTarget -PathType Container
    }
}

function Invoke-ManagerAction {
    param([Parameter(Mandatory = $true)][string]$RequestedAction)
    switch ($RequestedAction) {
        'status' { return Get-ManagerState }
        'enable' { $null = Set-ManagedStartup; return Get-ManagerState }
        'disable' { $null = Remove-ManagedStartup; return Get-ManagerState }
        'start' { return Start-ManagedService }
        'stop' { return Stop-ManagedService }
        'launch-codex' {
            $null = Invoke-ManagedScript -Path (Join-Path $PSScriptRoot 'launch-codex.ps1') -Arguments @('-DataDir', $installDir)
            return Get-ManagerState
        }
        'safe-disable' { return Invoke-ManagerSafeDisable }
        'uninstall' {
            if ($RemoveData -and -not $NoPrompt) {
                throw 'Destructive data removal requires -NoPrompt when invoked without the graphical manager.'
            }
            return Invoke-ManagerUninstall
        }
        default { throw "Unsupported manager action: $RequestedAction" }
    }
}

function Get-ManagerStateRows {
    param([Parameter(Mandatory = $true)]$State)
    $fields = @(
        @('PluginInstalled', 'state_plugin_installed'),
        @('PluginVersion', 'state_plugin_version'),
        @('InstallDir', 'state_install_dir'),
        @('ServiceRunning', 'state_service_running'),
        @('ProcessCount', 'state_process_count'),
        @('ProcessIds', 'state_process_ids'),
        @('HeartbeatFresh', 'state_heartbeat_fresh'),
        @('RuntimeVersion', 'state_runtime_version'),
        @('LastScanAt', 'state_last_scan'),
        @('StartupMode', 'state_startup_mode'),
        @('StartupEntry', 'state_startup_entry'),
        @('StartupOwned', 'state_startup_owned'),
        @('StartupApproved', 'state_startup_approved'),
        @('SharedModeEnabled', 'state_shared_enabled'),
        @('SharedModeRequested', 'state_shared_requested'),
        @('SharedEndpointConfigured', 'state_shared_endpoint'),
        @('DesktopLaunchMode', 'state_desktop_launch'),
        @('SafeLauncher', 'state_safe_launcher'),
        @('SharedServerState', 'state_shared_server'),
        @('SharedServerVerification', 'state_shared_verification'),
        @('SharedAppServerMemoryUsageMB', 'state_shared_memory'),
        @('SharedAppServerMemoryLimitMB', 'state_shared_memory_limit'),
        @('SharedAppServerMemoryGuardTriggered', 'state_shared_memory_guard'),
        @('RetrySafetyWarning', 'state_retry_warning'),
        @('StatusCompatibility', 'state_status_compatibility'),
        @('StatusCompatibilityMessage', 'state_status_message'),
        @('DataDirectoryExists', 'state_data_dir')
    )
    $rows = New-Object System.Collections.Generic.List[object]
    foreach ($field in $fields) {
        $property = $State.PSObject.Properties[[string]$field[0]]
        $value = if ($null -eq $property -or $null -eq $property.Value -or [string]::IsNullOrWhiteSpace([string]$property.Value)) {
            Get-ManagerText 'na'
        }
        elseif ($property.Value -is [bool]) {
            if ([bool]$property.Value) { Get-ManagerText 'yes' } else { Get-ManagerText 'no' }
        }
        elseif ($property.Value -is [System.Array]) {
            @($property.Value) -join ', '
        }
        else {
            [string]$property.Value
        }
        $rows.Add([pscustomobject]@{ Label = Get-ManagerText ([string]$field[1]); Value = $value })
    }
    return $rows.ToArray()
}

function Format-ManagerState {
    param([Parameter(Mandatory = $true)]$State)
    return ((Get-ManagerStateRows -State $State | ForEach-Object { '{0}: {1}' -f $_.Label, $_.Value }) -join [Environment]::NewLine)
}

function Hide-ManagerConsoleWindow {
    # The .cmd entry point is intentionally simple and may be hosted by
    # Windows Terminal. Hide only that console after the Forms window is ready;
    # hiding the PowerShell process itself would also hide the manager dialog.
    try {
        if (-not ('CodexAutoRetry.NativeWindow' -as [type])) {
            Add-Type @'
using System;
using System.Runtime.InteropServices;
namespace CodexAutoRetry {
    public static class NativeWindow {
        [DllImport("kernel32.dll")]
        public static extern IntPtr GetConsoleWindow();

        [DllImport("user32.dll")]
        [return: MarshalAs(UnmanagedType.Bool)]
        public static extern bool ShowWindow(IntPtr window, int command);
    }
}
'@
        }
        $console = [CodexAutoRetry.NativeWindow]::GetConsoleWindow()
        if ($console -ne [IntPtr]::Zero) {
            [void][CodexAutoRetry.NativeWindow]::ShowWindow($console, 0)
        }
    }
    catch {
        # A host without a console or without the native window API can still
        # use the graphical manager; hiding the console is only cosmetic.
    }
}

function Show-Manager {
    Add-Type -AssemblyName System.Windows.Forms
    Add-Type -AssemblyName System.Drawing

    $form = New-Object System.Windows.Forms.Form
    $form.Text = Get-ManagerText 'form_title'
    $form.StartPosition = 'CenterScreen'
    $form.Size = New-Object System.Drawing.Size(940, 720)
    $form.MinimumSize = New-Object System.Drawing.Size(840, 650)

    $title = New-Object System.Windows.Forms.Label
    $title.Text = Get-ManagerText 'form_title'
    $title.Font = New-Object System.Drawing.Font('Segoe UI', 14, [System.Drawing.FontStyle]::Bold)
    $title.AutoSize = $false
    $title.Location = New-Object System.Drawing.Point(18, 15)
    $title.Size = New-Object System.Drawing.Size(700, 34)
    $form.Controls.Add($title)

    $subtitle = New-Object System.Windows.Forms.Label
    $subtitle.Text = Get-ManagerText 'subtitle'
    $subtitle.AutoSize = $false
    $subtitle.ForeColor = [System.Drawing.Color]::DimGray
    $subtitle.Location = New-Object System.Drawing.Point(20, 52)
    $subtitle.Size = New-Object System.Drawing.Size(750, 38)
    $form.Controls.Add($subtitle)

    $language = New-Object System.Windows.Forms.Button
    $language.Text = Get-ManagerText 'language_button'
    $language.Anchor = 'Top,Right'
    $language.Location = New-Object System.Drawing.Point(775, 16)
    $language.Size = New-Object System.Drawing.Size(130, 34)
    $language.Font = New-Object System.Drawing.Font('Segoe UI', 9, [System.Drawing.FontStyle]::Bold)
    $form.Controls.Add($language)

    $output = New-Object System.Windows.Forms.DataGridView
    $output.ReadOnly = $true
    $output.AllowUserToAddRows = $false
    $output.AllowUserToDeleteRows = $false
    $output.AllowUserToResizeRows = $false
    $output.RowHeadersVisible = $false
    $output.AutoSizeRowsMode = 'AllCells'
    $output.SelectionMode = 'FullRowSelect'
    $output.MultiSelect = $false
    $output.BackgroundColor = [System.Drawing.SystemColors]::Window
    $output.BorderStyle = 'FixedSingle'
    $output.Font = New-Object System.Drawing.Font('Microsoft JhengHei UI', 9.25)
    $output.DefaultCellStyle.WrapMode = 'True'
    $output.DefaultCellStyle.Alignment = 'TopLeft'
    [void]$output.Columns.Add('Property', (Get-ManagerText 'column_item'))
    [void]$output.Columns.Add('Value', (Get-ManagerText 'column_value'))
    $output.Columns[0].Width = 230
    $output.Columns[0].Frozen = $true
    $output.Columns[1].AutoSizeMode = 'Fill'
    $output.Anchor = 'Top,Bottom,Left,Right'
    $output.Location = New-Object System.Drawing.Point(18, 95)
    $output.Size = New-Object System.Drawing.Size(886, 400)
    $form.Controls.Add($output)

    $buttons = New-Object System.Windows.Forms.FlowLayoutPanel
    $buttons.FlowDirection = 'LeftToRight'
    $buttons.WrapContents = $true
    $buttons.Anchor = 'Bottom,Left,Right'
    $buttons.Location = New-Object System.Drawing.Point(18, 510)
    $buttons.Size = New-Object System.Drawing.Size(886, 140)
    $buttons.Padding = New-Object System.Windows.Forms.Padding(0)
    $form.Controls.Add($buttons)

    $refresh = New-Object System.Windows.Forms.Button
    $refresh.Text = Get-ManagerText 'refresh_status'
    $refresh.Width = 210
    $refresh.Height = 38
    $refresh.Font = New-Object System.Drawing.Font('Segoe UI', 9, [System.Drawing.FontStyle]::Bold)
    $buttons.Controls.Add($refresh)

    $definitions = @(
        @('launch_codex', 'launch-codex', $false),
        @('enable_startup', 'enable', $false),
        @('disable_startup', 'disable', $false),
        @('start_service', 'start', $false),
        @('stop_service', 'stop', $false),
        @('safe_disable', 'safe-disable', $true),
        @('uninstall_keep', 'uninstall', $true),
        @('uninstall_remove', 'uninstall-remove', $true)
    )

    $managerButtons = @{}

    $refreshView = {
        try {
            $output.Rows.Clear()
            foreach ($row in @(Get-ManagerStateRows -State (Get-ManagerState))) {
                [void]$output.Rows.Add([string]$row.Label, [string]$row.Value)
            }
        }
        catch {
            $output.Rows.Clear()
            [void]$output.Rows.Add((Get-ManagerText 'action_failed'), $_.Exception.Message)
        }
    }.GetNewClosure()

    function Add-ManagerButton {
        param([string]$TextKey, [string]$ButtonAction, [bool]$Danger, [scriptblock]$RefreshView)
        $button = New-Object System.Windows.Forms.Button
        $button.Text = Get-ManagerText $TextKey
        $button.Width = 210
        $button.Height = 38
        $button.Font = New-Object System.Drawing.Font('Segoe UI', 9, [System.Drawing.FontStyle]::Bold)
        if ($Danger) { $button.ForeColor = [System.Drawing.Color]::DarkRed }
        $handler = {
            try {
                if ($ButtonAction -eq 'uninstall-remove') {
                    $confirm = [System.Windows.Forms.MessageBox]::Show(
                        (Get-ManagerText 'confirm_remove_message'),
                        (Get-ManagerText 'confirm_remove_title'),
                        [System.Windows.Forms.MessageBoxButtons]::YesNo,
                        [System.Windows.Forms.MessageBoxIcon]::Warning
                    )
                    if ($confirm -ne [System.Windows.Forms.DialogResult]::Yes) { return }
                    $script:RemoveData = $true
                    $script:NoPrompt = $true
                    $null = Invoke-ManagerAction -RequestedAction 'uninstall'
                    [System.Windows.Forms.MessageBox]::Show((Get-ManagerText 'full_uninstall_done'), 'CodexWorkResumer') | Out-Null
                    $form.Close()
                    return
                }
                if ($ButtonAction -eq 'uninstall') {
                    $confirm = [System.Windows.Forms.MessageBox]::Show(
                        (Get-ManagerText 'confirm_uninstall_message'),
                        (Get-ManagerText 'confirm_uninstall_title'),
                        [System.Windows.Forms.MessageBoxButtons]::YesNo,
                        [System.Windows.Forms.MessageBoxIcon]::Question
                    )
                    if ($confirm -ne [System.Windows.Forms.DialogResult]::Yes) { return }
                    $script:RemoveData = $false
                    $script:NoPrompt = $true
                    $null = Invoke-ManagerAction -RequestedAction 'uninstall'
                    [System.Windows.Forms.MessageBox]::Show((Get-ManagerText 'uninstall_done'), 'CodexWorkResumer') | Out-Null
                    $form.Close()
                    return
                }
                $null = Invoke-ManagerAction -RequestedAction $ButtonAction
                & $RefreshView
            }
            catch {
                [System.Windows.Forms.MessageBox]::Show($_.Exception.Message, (Get-ManagerText 'action_failed'), [System.Windows.Forms.MessageBoxButtons]::OK, [System.Windows.Forms.MessageBoxIcon]::Error) | Out-Null
                try { & $RefreshView } catch { $output.Rows.Clear(); [void]$output.Rows.Add((Get-ManagerText 'action_failed'), $_.Exception.Message) }
            }
        }.GetNewClosure()
        $button.Add_Click($handler)
        $buttons.Controls.Add($button)
        $managerButtons[$ButtonAction] = [pscustomobject]@{ Button = $button; TextKey = $TextKey }
    }

    foreach ($definition in $definitions) {
        Add-ManagerButton -TextKey $definition[0] -ButtonAction $definition[1] -Danger ([bool]$definition[2]) -RefreshView $refreshView
    }
    $applyLanguage = {
        $form.Text = Get-ManagerText 'form_title'
        $title.Text = Get-ManagerText 'form_title'
        $subtitle.Text = Get-ManagerText 'subtitle'
        $language.Text = Get-ManagerText 'language_button'
        $refresh.Text = Get-ManagerText 'refresh_status'
        $output.Columns[0].HeaderText = Get-ManagerText 'column_item'
        $output.Columns[1].HeaderText = Get-ManagerText 'column_value'
        foreach ($entry in $managerButtons.Values) {
            $entry.Button.Text = Get-ManagerText $entry.TextKey
        }
    }.GetNewClosure()
    $refresh.Add_Click({ & $refreshView }.GetNewClosure())
    $language.Add_Click({
        Switch-ManagerLanguage
        & $applyLanguage
        & $refreshView
    }.GetNewClosure())
    $form.Add_Shown({
        Hide-ManagerConsoleWindow
        # A hidden script host can also suppress the first ShowWindow call.
        # Explicitly show this dialog, never the console or a Codex window.
        if ('CodexAutoRetry.NativeWindow' -as [type]) {
            [void][CodexAutoRetry.NativeWindow]::ShowWindow($form.Handle, 1)
        }
        & $applyLanguage
        & $refreshView
        $form.Activate()
    }.GetNewClosure())
    [void]$form.ShowDialog()
}

if ($Action -eq 'gui') {
    Show-Manager
    return
}

$result = Invoke-ManagerAction -RequestedAction $Action
if ($null -ne $result) { $result | Format-List * }
