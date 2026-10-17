param([Parameter(Mandatory=$true)][string]$Server, [string]$Token, [string]$DeviceCode)

$ErrorActionPreference = "Stop"
$Server = $Server.TrimEnd('/')
$WeaveDir = Join-Path $env:USERPROFILE ".weave"
$BinDir = Join-Path $WeaveDir "bin"
$Bin = Join-Path $BinDir "weave-runtime.exe"
$Arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    "AMD64" { "amd64" }
    "ARM64" { "arm64" }
    default { throw "Unsupported architecture: $env:PROCESSOR_ARCHITECTURE" }
}
if (-not $Token -and -not $DeviceCode) { throw "Give -Token for a runtime or -DeviceCode for a device." }

# Download fetches a file and refuses it unless its SHA-256 matches the
# X-Weave-SHA256 header. A server too old to send the header is accepted for
# runtime installs only; device installs require it.
function Download([string]$Uri, [string]$OutFile) {
    $response = Invoke-WebRequest -Uri $Uri -OutFile $OutFile -PassThru -UseBasicParsing
    $expected = "$($response.Headers['X-Weave-SHA256'])".Trim()
    if (-not $expected) {
        if ($DeviceCode) { throw "The server did not send a checksum for $Uri; nothing was installed." }
        return
    }
    $actual = (Get-FileHash -Algorithm SHA256 -Path $OutFile).Hash.ToLowerInvariant()
    if ($actual -ne $expected.ToLowerInvariant()) { throw "Checksum mismatch for $Uri; nothing was installed." }
}

function Restrict([string]$Path) {
    icacls $Path /inheritance:r /grant:r "${env:USERNAME}:(R,W)" | Out-Null
}

New-Item -ItemType Directory -Force -Path $BinDir | Out-Null

if ($DeviceCode) {
    # This PC becomes the person's own device: the Weave node and the
    # cua-driver release the server pins, both started at logon, bound to the
    # account that issued the code (weave-workbench decision 009).
    $EnvFile = Join-Path $WeaveDir "device.env"
    $DriverDir = Join-Path $env:LOCALAPPDATA "Programs\Cua\cua-driver"
    $Work = Join-Path ([IO.Path]::GetTempPath()) ("weave-device-" + [Guid]::NewGuid().ToString("N"))
    New-Item -ItemType Directory -Force -Path $Work | Out-Null
    try {
        Write-Host "Downloading the Weave node and cua-driver..."
        Download "$Server/v1/downloads/runtime/windows/$Arch" (Join-Path $Work "weave-runtime.exe")
        Download "$Server/v1/downloads/cua-driver/windows/$Arch" (Join-Path $Work "cua-driver.zip")
        Download "$Server/v1/downloads/cua-driver/license/notice" (Join-Path $Work "LICENSE")
        Expand-Archive -Path (Join-Path $Work "cua-driver.zip") -DestinationPath (Join-Path $Work "driver") -Force
        $NewDriver = Get-ChildItem -Path (Join-Path $Work "driver") -Recurse -Filter "cua-driver.exe" | Select-Object -First 1
        if (-not $NewDriver) { throw "The cua-driver archive has no cua-driver.exe; nothing was installed." }

        # Exchange the code before touching the machine, so a wrong or used
        # code leaves nothing behind.
        $PreviousId = ""
        if (Test-Path $EnvFile) {
            $line = Get-Content $EnvFile | Where-Object { $_ -like "WEAVE_RUNTIME_ID=*" } | Select-Object -First 1
            if ($line) { $PreviousId = $line.Substring("WEAVE_RUNTIME_ID=".Length) }
        }
        $Request = @{ code = $DeviceCode; hostname = $env:COMPUTERNAME; platform = "windows"; arch = $Arch; runtime_id = $PreviousId } | ConvertTo-Json -Compress
        try {
            $Reply = Invoke-RestMethod -Method Post -Uri "$Server/v1/runtime/device-enrollments" -ContentType "application/json; charset=utf-8" -Body ([Text.Encoding]::UTF8.GetBytes($Request))
        } catch {
            $message = $_.ErrorDetails.Message
            try { $message = ($message | ConvertFrom-Json).error } catch {}
            if (-not $message) { $message = $_.Exception.Message }
            throw "Registration failed: $message"
        }

        $Driver = Join-Path $DriverDir "cua-driver.exe"
        if (Test-Path $Driver) { & $Driver stop 2>$null | Out-Null }
        New-Item -ItemType Directory -Force -Path $DriverDir | Out-Null
        Copy-Item -Path (Join-Path $NewDriver.DirectoryName "*") -Destination $DriverDir -Recurse -Force
        Copy-Item -Path (Join-Path $Work "LICENSE") -Destination (Join-Path $DriverDir "LICENSE") -Force
        Move-Item -Path (Join-Path $Work "weave-runtime.exe") -Destination $Bin -Force

        @("WEAVE_SERVER=$Server", "WEAVE_RUNTIME_TOKEN=$($Reply.token)", "WEAVE_RUNTIME_ID=$($Reply.runtime_id)", "WEAVE_CUA_DRIVER=$Driver") | Set-Content -Path $EnvFile
        Restrict $EnvFile
    } finally {
        Remove-Item -Path $Work -Recurse -Force -ErrorAction SilentlyContinue
    }

    # cua-driver registers its own logon task with the highest run level, so
    # it can drive elevated windows; that asks for elevation once.
    $elevated = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
    if ($elevated) {
        & $Driver autostart enable | Out-Null
    } else {
        $proc = Start-Process -FilePath $Driver -ArgumentList @("autostart", "enable") -Verb RunAs -Wait -PassThru
        if ($proc.ExitCode -ne 0) { Write-Warning "cua-driver could not register its logon task; run '$Driver autostart enable' as administrator." }
    }
    & $Driver autostart kick | Out-Null

    $Arguments = "runtime --server `"$Server`" --runtime-token `"$($Reply.token)`" --device --cua-driver `"$Driver`""
    $Action = New-ScheduledTaskAction -Execute $Bin -Argument $Arguments
    $Trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
    Register-ScheduledTask -TaskName "WeaveDevice" -Action $Action -Trigger $Trigger -Force | Out-Null
    Start-ScheduledTask -TaskName "WeaveDevice"

    Write-Host "设备已注册：$($Reply.name)"
    Write-Host "Status: Get-ScheduledTask -TaskName WeaveDevice | Get-ScheduledTaskInfo"
    return
}

$EnvFile = Join-Path $WeaveDir "runtime.env"
$TempBin = Join-Path $BinDir (".weave-runtime.{0}.tmp" -f [Guid]::NewGuid().ToString("N"))
try {
    Download "$Server/v1/downloads/runtime/windows/$Arch" $TempBin
    Move-Item -Path $TempBin -Destination $Bin -Force
} finally {
    if (Test-Path $TempBin) {
        Remove-Item -Path $TempBin -Force
    }
}
@("WEAVE_SERVER=$Server", "WEAVE_RUNTIME_TOKEN=$Token") | Set-Content -Path $EnvFile
Restrict $EnvFile

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
