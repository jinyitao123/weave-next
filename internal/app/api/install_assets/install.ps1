param([Parameter(Mandatory=$true)][string]$Server, [Parameter(Mandatory=$true)][string]$Token)

$ErrorActionPreference = "Stop"
$Server = $Server.TrimEnd('/')
$WeaveDir = Join-Path $env:USERPROFILE ".weave"
$BinDir = Join-Path $WeaveDir "bin"
$Bin = Join-Path $BinDir "weave-runtime.exe"
$EnvFile = Join-Path $WeaveDir "runtime.env"
$Arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    "AMD64" { "amd64" }
    "ARM64" { "arm64" }
    default { throw "Unsupported architecture: $env:PROCESSOR_ARCHITECTURE" }
}

New-Item -ItemType Directory -Force -Path $BinDir | Out-Null
$TempBin = Join-Path $BinDir (".weave-runtime.{0}.tmp" -f [Guid]::NewGuid().ToString("N"))
try {
    Invoke-WebRequest -Uri "$Server/v1/downloads/runtime/windows/$Arch" -OutFile $TempBin
    Move-Item -Path $TempBin -Destination $Bin -Force
} finally {
    if (Test-Path $TempBin) {
        Remove-Item -Path $TempBin -Force
    }
}
@("WEAVE_SERVER=$Server", "WEAVE_RUNTIME_TOKEN=$Token") | Set-Content -Path $EnvFile
icacls $EnvFile /inheritance:r /grant:r "${env:USERNAME}:(R,W)" | Out-Null

$Arguments = "runtime --server `"$Server`" --runtime-token `"$Token`""
$Action = New-ScheduledTaskAction -Execute $Bin -Argument $Arguments
$Trigger = New-ScheduledTaskTrigger -AtLogOn
try {
    Register-ScheduledTask -TaskName "WeaveRuntime" -Action $Action -Trigger $Trigger -Force | Out-Null
    Start-ScheduledTask -TaskName "WeaveRuntime"
    Write-Host "Background service: scheduled task WeaveRuntime"
} catch {
    Write-Warning "Could not register or start the scheduled task. Start it manually:"
    Write-Host "& `"$Bin`" runtime --server `"$Server`" --runtime-token `"$Token`""
}

Write-Host "Installed Weave runtime at $Bin"
Write-Host "Status: Get-ScheduledTask -TaskName WeaveRuntime | Get-ScheduledTaskInfo"
Write-Host "Logs: Task Scheduler > Task Scheduler Library > WeaveRuntime > History"
Write-Host "Return to Workbench runtime settings to check the node connection status."
