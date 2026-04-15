<#
.SYNOPSIS
  Download and install the `viti` CLI on Windows (optionally with viti-gui).

.DESCRIPTION
  Mirrors install.sh (Linux/macOS). Detects architecture, downloads the
  matching release .zip from GitHub, verifies the SHA-256 checksum and
  (if cosign is on PATH) the Sigstore keyless signature, extracts into
  the install prefix, and appends the prefix to the user's PATH.

.PARAMETER Version
  Release tag to install (default: latest).

.PARAMETER Prefix
  Install directory (default: $env:LOCALAPPDATA\Programs\viti).

.PARAMETER WithGui
  Also install the viti-gui plugin binary.

.PARAMETER SkipCosign
  Skip Sigstore signature verification (SHA-256 is still enforced).

.PARAMETER SkipChecksum
  Skip SHA-256 verification (not recommended).

.PARAMETER NoPathUpdate
  Don't append the install prefix to the user PATH.

.EXAMPLE
  irm https://raw.githubusercontent.com/vitistack/vitictl/main/install.ps1 | iex

.EXAMPLE
  & ([scriptblock]::Create((irm https://raw.githubusercontent.com/vitistack/vitictl/main/install.ps1))) -WithGui

.EXAMPLE
  .\install.ps1 -Prefix "$env:USERPROFILE\bin" -SkipCosign
#>

[CmdletBinding()]
param(
    [string]$Version,
    [string]$Prefix,
    [switch]$WithGui,
    [switch]$SkipCosign,
    [switch]$SkipChecksum,
    [switch]$NoPathUpdate
)

$ErrorActionPreference = 'Stop'
# PowerShell 5.1 defaults to TLS 1.0/1.1; GitHub requires TLS 1.2+.
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

$Repo      = 'vitistack/vitictl'
$Binary    = 'viti'
$BinaryGui = 'viti-gui'

function Write-Log  { param([string]$m) Write-Host "==> $m" -ForegroundColor Cyan }
function Write-Warn { param([string]$m) Write-Host "warning: $m" -ForegroundColor Yellow }
function Die        { param([string]$m) Write-Host "error: $m" -ForegroundColor Red; exit 1 }

# -- Detect architecture -----------------------------------------------------
# PROCESSOR_ARCHITEW6432 is set when a 32-bit PowerShell is running on a
# 64-bit host; its value reflects the true machine arch. Prefer it when set.
$archRaw = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
switch ($archRaw) {
    'AMD64' { $Arch = 'amd64' }
    'ARM64' { $Arch = 'arm64' }
    default { Die "unsupported architecture: $archRaw" }
}
$Os = 'windows'

# -- Resolve version ---------------------------------------------------------
if (-not $Version) {
    Write-Log 'resolving latest release'
    try {
        $release = Invoke-RestMethod -Headers @{ 'Accept' = 'application/vnd.github+json' } `
            -Uri "https://api.github.com/repos/$Repo/releases/latest"
        $Version = $release.tag_name
    } catch {
        Die "could not query latest release: $($_.Exception.Message)"
    }
    if (-not $Version) { Die "could not determine latest release tag for $Repo" }
}

$BaseUrl   = "https://github.com/$Repo/releases/download/$Version"
$Checksums = "viti-$Version-SHA256SUMS"

# -- Pick install prefix -----------------------------------------------------
if (-not $Prefix) {
    $Prefix = Join-Path $env:LOCALAPPDATA 'Programs\viti'
}
New-Item -ItemType Directory -Force -Path $Prefix | Out-Null

# -- Download into temp dir --------------------------------------------------
$Tmp = Join-Path $env:TEMP ("viti-install-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force -Path $Tmp | Out-Null

# Download SHA256SUMS once; reused across artifacts.
$sumsPath = $null
if (-not $SkipChecksum) {
    $sumsPath = Join-Path $Tmp $Checksums
    Invoke-WebRequest -Uri "$BaseUrl/$Checksums" -OutFile $sumsPath -UseBasicParsing
} else {
    Write-Warn 'skipping SHA-256 verification'
}
if ($SkipCosign) { Write-Warn 'skipping cosign signature verification' }

function Install-One {
    param([string]$Bin)

    $asset     = "$Bin-$Version-$Os-$Arch.zip"
    $assetPath = Join-Path $Tmp $asset

    Write-Log "installing $Bin $Version for $Os/$Arch"
    Write-Log "downloading $asset"
    try {
        Invoke-WebRequest -Uri "$BaseUrl/$asset" -OutFile $assetPath -UseBasicParsing
    } catch {
        Die "download failed for $asset : $($_.Exception.Message)"
    }

    # SHA-256
    if (-not $SkipChecksum) {
        $expected = $null
        foreach ($line in Get-Content $sumsPath) {
            $parts = $line -split '\s+', 2
            if ($parts.Count -ne 2) { continue }
            $file = ($parts[1]).TrimStart('*').Trim()
            if ($file -eq $asset) { $expected = $parts[0].Trim(); break }
        }
        if (-not $expected) { Die "no SHA-256 entry for $asset in $Checksums" }

        $actual = (Get-FileHash -Algorithm SHA256 -Path $assetPath).Hash.ToLower()
        if ($expected.ToLower() -ne $actual) {
            Die "SHA-256 mismatch for $asset : expected $expected, got $actual"
        }
        Write-Log "SHA-256 ok ($Bin)"
    }

    # Cosign
    if (-not $SkipCosign) {
        $cosign = Get-Command cosign -ErrorAction SilentlyContinue
        if (-not $cosign) {
            Write-Warn "cosign not found on PATH — skipping signature verification for $Bin. Install cosign (https://docs.sigstore.dev/cosign/installation/) or re-run with -SkipCosign to silence this warning."
        } else {
            Write-Log "verifying Sigstore signature with cosign ($Bin)"
            $bundlePath = Join-Path $Tmp "$asset.cosign.bundle"
            Invoke-WebRequest -Uri "$BaseUrl/$asset.cosign.bundle" -OutFile $bundlePath -UseBasicParsing

            $identityRegex = "^https://github.com/$Repo/.github/workflows/release.yml@refs/tags/"
            & $cosign.Source verify-blob `
                --bundle $bundlePath `
                --certificate-identity-regexp $identityRegex `
                --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' `
                $assetPath | Out-Null
            if ($LASTEXITCODE -ne 0) { Die "cosign verification failed for $Bin" }
            Write-Log "cosign signature ok ($Bin)"
        }
    }

    # Extract + install
    Write-Log "extracting $Bin"
    $extractDir = Join-Path $Tmp "extract-$Bin"
    Expand-Archive -Path $assetPath -DestinationPath $extractDir -Force

    $staged = Join-Path $extractDir "$Bin-$Version-$Os-$Arch\$Bin.exe"
    if (-not (Test-Path $staged)) { Die "archive layout unexpected: $staged not found" }

    $dest = Join-Path $Prefix "$Bin.exe"
    Copy-Item -Force -Path $staged -Destination $dest
    Write-Log "installed $dest"
    return $dest
}

try {
    $vitiDest = Install-One -Bin $Binary
    if ($WithGui) { Install-One -Bin $BinaryGui | Out-Null }

    # -- Update user PATH ----------------------------------------------------
    if (-not $NoPathUpdate) {
        $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
        $parts = @()
        if ($userPath) { $parts = $userPath -split ';' | Where-Object { $_ -ne '' } }
        $already = $parts | Where-Object { $_.TrimEnd('\') -ieq $Prefix.TrimEnd('\') }
        if (-not $already) {
            $newPath = ($parts + $Prefix) -join ';'
            [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
            Write-Log "added $Prefix to user PATH — open a new terminal for it to take effect"
        } else {
            Write-Log "$Prefix is already on user PATH"
        }
    }

    # -- Post-install smoke check --------------------------------------------
    try {
        & $vitiDest --help *> $null
        Write-Log "run '$Binary --help' to get started"
    } catch {
        Write-Warn "$Binary installed but failed to run; check architecture mismatch or corrupted download"
    }
    if ($WithGui) {
        Write-Log "run 'viti gui' (viti will dispatch to $BinaryGui)"
    }
} finally {
    Remove-Item -Recurse -Force -ErrorAction SilentlyContinue -Path $Tmp
}
