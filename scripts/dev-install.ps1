<#
.SYNOPSIS
  Install (or remove) the built contenox CLI on the current user's PATH.

.DESCRIPTION
  The Windows half of the Taskfile's dev-link / dev-unlink targets.

  Windows has no equivalent of the ~/.local/bin convention the POSIX branch
  relies on, so "put it on the PATH" cannot be satisfied by placing a file
  alone: the directory has to be registered too. This installs to
  %LOCALAPPDATA%\Programs\contenox and adds that directory to the USER PATH
  (never the machine PATH, which would need elevation and affect other users).
  Both halves are idempotent: re-running installs over the existing copy and
  leaves an already-present PATH entry alone.

  It copies rather than symlinks on purpose. A symlink on Windows needs either
  Developer Mode or an elevated shell, so a link-based install fails for the
  common case; the cost is that the copy is a snapshot and `task dev-install`
  must be re-run after a rebuild.
#>
[CmdletBinding()]
param(
    # The freshly built binary to install. Ignored with -Uninstall.
    [string]$Source,
    # Remove the installed copy and drop the directory from the user PATH.
    [switch]$Uninstall
)

$ErrorActionPreference = 'Stop'

$dir = Join-Path $env:LOCALAPPDATA 'Programs\contenox'
$target = Join-Path $dir 'contenox.exe'

function Get-UserPathEntries {
    $raw = [Environment]::GetEnvironmentVariable('Path', 'User')
    if ([string]::IsNullOrEmpty($raw)) { return @() }
    return @($raw -split ';' | Where-Object { $_ -ne '' })
}

if ($Uninstall) {
    if (Test-Path -LiteralPath $target) {
        Remove-Item -LiteralPath $target -Force
        Write-Host "Removed $target"
    } else {
        Write-Host "Nothing to remove at $target"
    }

    $entries = Get-UserPathEntries
    if ($entries -contains $dir) {
        $kept = @($entries | Where-Object { $_ -ne $dir })
        [Environment]::SetEnvironmentVariable('Path', ($kept -join ';'), 'User')
        Write-Host "Removed $dir from your user PATH (open a new terminal for it to take effect)."
    }
    exit 0
}

if ([string]::IsNullOrWhiteSpace($Source)) {
    throw '-Source is required when installing'
}
if (-not (Test-Path -LiteralPath $Source)) {
    throw "built binary not found at $Source - run 'task build' first"
}

New-Item -ItemType Directory -Force -Path $dir | Out-Null
Copy-Item -LiteralPath $Source -Destination $target -Force
Write-Host "Installed $target"

$entries = Get-UserPathEntries
if ($entries -contains $dir) {
    Write-Host "$dir is already on your user PATH."
    Write-Host "Run: contenox doctor"
} else {
    [Environment]::SetEnvironmentVariable('Path', (@($entries) + $dir -join ';'), 'User')
    Write-Host "Added $dir to your user PATH."
    Write-Host "Open a NEW terminal (this one keeps the old PATH), then run: contenox doctor"
}
