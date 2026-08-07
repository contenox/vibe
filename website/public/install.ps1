<#
.SYNOPSIS
  Contenox installer for Windows.

.DESCRIPTION
  The Windows half of install.sh, with the same contract: resolve the latest
  release, download the matching binary, verify it against the release's
  SHA256SUMS manifest, and only then put it on the PATH. A missing manifest,
  a missing entry, or a mismatch aborts and installs nothing.

  Windows has no /usr/local/bin convention, so "on the PATH" means installing
  to %LOCALAPPDATA%\Programs\contenox and registering that directory in the
  USER PATH (never the machine PATH, which would need elevation).

  This file is deliberately pure ASCII: Windows PowerShell 5.1 reads a
  BOM-less script as ANSI, and non-ASCII characters corrupt on the way in.

.EXAMPLE
  irm https://contenox.com/install.ps1 | iex
#>

$ErrorActionPreference = 'Stop'

function Install-Contenox {
    $repo = 'contenox/contenox'

    # PS 5.1 defaults to TLS 1.0/1.1; github.com refuses those.
    try {
        [Net.ServicePointManager]::SecurityProtocol =
            [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    } catch {
        # PowerShell 7 negotiates through the OS and does not expose this knob.
    }

    # Invoke-WebRequest's progress bar makes downloads an order of magnitude
    # slower on PS 5.1. Restored in the finally block.
    $priorProgress = $ProgressPreference
    $ProgressPreference = 'SilentlyContinue'

    $tmpExe = $null
    $tmpSums = $null

    try {
        # -- Detect arch ---------------------------------------------------------
        # A 32-bit PowerShell on 64-bit Windows reports x86 here and puts the
        # real architecture in PROCESSOR_ARCHITEW6432.
        $raw = $env:PROCESSOR_ARCHITEW6432
        if ([string]::IsNullOrEmpty($raw)) { $raw = $env:PROCESSOR_ARCHITECTURE }

        switch ($raw.ToUpperInvariant()) {
            'AMD64' { $goarch = 'amd64' }
            'ARM64' { $goarch = 'arm64' }
            default {
                Write-Host "Unsupported architecture: $raw"
                Write-Host "Please download manually from https://github.com/$repo/releases"
                throw "unsupported architecture: $raw"
            }
        }

        # -- Fetch latest release tag -------------------------------------------
        # Resolved from the releases/latest redirect (not the GitHub API, which
        # is rate-limited for unauthenticated callers).
        Write-Host 'Fetching latest Contenox release...'
        $tag = Get-LatestTag -Repo $repo
        if ([string]::IsNullOrWhiteSpace($tag)) {
            Write-Host 'Error: could not determine latest release tag.'
            Write-Host "Please download manually from https://github.com/$repo/releases"
            throw 'could not determine latest release tag'
        }
        Write-Host "Latest version: $tag"

        # -- Download binary -----------------------------------------------------
        $asset = "contenox-windows-$goarch.exe"
        $url = "https://github.com/$repo/releases/download/$tag/$asset"
        $tmpExe = Join-Path ([IO.Path]::GetTempPath()) ([IO.Path]::GetRandomFileName())

        Write-Host "Downloading $asset..."
        Invoke-WebRequest -Uri $url -OutFile $tmpExe -UseBasicParsing -TimeoutSec 600

        # -- Verify against the release checksum manifest ------------------------
        # Fails closed: no manifest, no entry, or a mismatch aborts the install.
        # Nothing is copied into the install directory before this passes.
        $sumsUrl = "https://github.com/$repo/releases/download/$tag/SHA256SUMS"
        $tmpSums = Join-Path ([IO.Path]::GetTempPath()) ([IO.Path]::GetRandomFileName())

        Write-Host 'Verifying checksum...'
        try {
            Invoke-WebRequest -Uri $sumsUrl -OutFile $tmpSums -UseBasicParsing -TimeoutSec 600
        } catch {
            Write-Host ''
            Write-Host "Error: could not download SHA256SUMS for $tag."
            Write-Host 'Refusing to install an unverified binary.'
            Write-Host "  expected: $sumsUrl"
            Write-Host 'Releases before checksums were published do not carry this file.'
            Write-Host 'Install a newer release, or download and verify manually from'
            Write-Host "  https://github.com/$repo/releases"
            throw "SHA256SUMS not available for $tag"
        }

        $expected = $null
        foreach ($line in (Get-Content -LiteralPath $tmpSums)) {
            # sha256sum output: "<hex>  <name>", with a leading '*' on the name
            # in binary mode.
            $fields = $line.Trim() -split '\s+', 2
            if ($fields.Count -ne 2) { continue }
            $name = $fields[1].TrimStart('*').Trim()
            if ($name -eq $asset) { $expected = $fields[0].Trim(); break }
        }

        if ([string]::IsNullOrWhiteSpace($expected)) {
            Write-Host ''
            Write-Host "Error: SHA256SUMS for $tag has no entry for $asset."
            Write-Host 'Refusing to install an unverified binary.'
            throw "no SHA256SUMS entry for $asset"
        }

        $expected = $expected.ToLowerInvariant()
        $actual = (Get-FileHash -LiteralPath $tmpExe -Algorithm SHA256).Hash.ToLowerInvariant()

        if ($actual -ne $expected) {
            Write-Host ''
            Write-Host "Error: CHECKSUM MISMATCH for $asset."
            Write-Host "  expected: $expected"
            Write-Host "  actual:   $actual"
            Write-Host ''
            Write-Host 'The downloaded file does not match the published release. This could be'
            Write-Host 'a corrupted download or a tampered artifact. Nothing was installed.'
            Write-Host "Report it at https://github.com/$repo/security/advisories"
            throw "checksum mismatch for $asset"
        }

        Write-Host "OK checksum verified (sha256:$actual)"

        # -- Install -------------------------------------------------------------
        $dir = Join-Path $env:LOCALAPPDATA 'Programs\contenox'
        $target = Join-Path $dir 'contenox.exe'

        New-Item -ItemType Directory -Force -Path $dir | Out-Null
        try {
            Copy-Item -LiteralPath $tmpExe -Destination $target -Force
        } catch {
            Write-Host ''
            Write-Host "Error: could not write $target."
            Write-Host 'If contenox is currently running, close it and re-run this installer.'
            throw $_
        }

        Write-Host ''
        Write-Host "contenox $tag installed to $target"

        # -- Register the directory on the user PATH ------------------------------
        # Idempotent: an already-present entry is left alone. -contains is
        # case-insensitive, which is what PATH comparison wants on Windows.
        $entries = Get-UserPathEntries
        if ($entries -contains $dir) {
            Write-Host "$dir is already on your user PATH."
        } else {
            [Environment]::SetEnvironmentVariable('Path', ((@($entries) + $dir) -join ';'), 'User')
            Write-Host "Added $dir to your user PATH."
            Write-Host 'Open a NEW terminal (this one keeps the old PATH) before running contenox.'
        }

        Write-Host ''
        Write-Host 'Get started:'
        Write-Host '  contenox setup                        # pick a provider/model (local models or a hosted API)'
        Write-Host '  contenox init                         # scaffold a workspace in your project directory'
        Write-Host '  contenox "say hello world in python"   # run a prompt'
        Write-Host '  contenox acp                          # speak ACP over stdio (Zed, JetBrains, AionUi)'
    } finally {
        foreach ($f in @($tmpExe, $tmpSums)) {
            if ($f -and (Test-Path -LiteralPath $f)) {
                Remove-Item -LiteralPath $f -Force -ErrorAction SilentlyContinue
            }
        }
        $ProgressPreference = $priorProgress
    }
}

# Get-LatestTag reads the tag out of the Location header of the releases/latest
# redirect. HttpWebRequest is used rather than Invoke-WebRequest because
# suppressing redirects behaves differently on PS 5.1 and PS 7.
function Get-LatestTag {
    param([Parameter(Mandatory = $true)][string]$Repo)

    $req = [Net.HttpWebRequest]::Create("https://github.com/$Repo/releases/latest")
    $req.Method = 'HEAD'
    $req.AllowAutoRedirect = $false
    $req.UserAgent = 'contenox-install'
    $req.Timeout = 60000

    $resp = $null
    try {
        $resp = $req.GetResponse()
        $location = $resp.Headers['Location']
    } finally {
        if ($resp) { $resp.Close() }
    }

    if ([string]::IsNullOrWhiteSpace($location)) { return $null }

    $i = $location.LastIndexOf('/tag/')
    if ($i -lt 0) { return $null }
    return $location.Substring($i + 5).Trim()
}

# Get-UserPathEntries returns the user PATH as an array, empty entries dropped.
function Get-UserPathEntries {
    $raw = [Environment]::GetEnvironmentVariable('Path', 'User')
    if ([string]::IsNullOrEmpty($raw)) { return @() }
    return @($raw -split ';' | Where-Object { $_ -ne '' })
}

Install-Contenox
