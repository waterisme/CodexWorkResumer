[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$files = @(
    '.codex-plugin\plugin.json',
    'scripts\source\config.go',
    'scripts\source\management.go',
    'scripts\source\mcp_server.go',
    'scripts\source\memory_alert_windows.go',
    'scripts\source\tray_windows.go',
    'scripts\source\ui_state.go',
    'scripts\source\ui\panel.html',
    'scripts\source\ui\settings.ps1',
    'scripts\source\ui\src\panel.ts'
)
$simplifiedPattern = '当前|读取|后台|登录|启动|关闭|运行|队列|执行|扫描|状态|倒计时|恢复|连续|类型|脚本|插件|自动|启用|设置|保存|检查|失败|端口|任务|内存|连接|请求|默认|分钟|小时|达到|超过|遮挡|无法|供应商|回复|分类|准备|目标|处理|暂停|监控'
$legacyBackendPattern = '共享后台|共享後台|共享 Codex 后台|共享 Codex 後台'
$violations = New-Object System.Collections.Generic.List[string]

foreach ($relative in $files) {
    $path = Join-Path $root $relative
    $lineNumber = 0
    foreach ($line in Get-Content -LiteralPath $path -Encoding UTF8) {
        $lineNumber++
        if ($line -match $simplifiedPattern -or $line -match $legacyBackendPattern) {
            [void]$violations.Add("${relative}:${lineNumber}: $line")
        }
    }
}

if ($violations.Count -gt 0) {
    throw "Traditional Chinese audit failed:`n$($violations -join [Environment]::NewLine)"
}

[pscustomobject]@{
    Status = 'passed'
    FilesChecked = $files.Count
    Language = 'zh-Hant-TW'
}
