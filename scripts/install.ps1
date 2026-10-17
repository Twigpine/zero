[CmdletBinding()]
param(
  [string]$Version = $env:ZERO_VERSION,
  [string]$Repository = $(if ($env:ZERO_REPO) { $env:ZERO_REPO } else { "Twigpine/zero" }),
  [string]$InstallDir = $env:ZERO_INSTALL_DIR,
  [string]$GitHubApi = $(if ($env:ZERO_GITHUB_API) { $env:ZERO_GITHUB_API } else { "https://api.github.com" }),
  [string]$GitHubBaseUrl = $(if ($env:ZERO_GITHUB_BASE_URL) { $env:ZERO_GITHUB_BASE_URL } else { "https://github.com" })
)

$ErrorActionPreference = "Stop"

if ([string]::IsNullOrWhiteSpace($Version)) {
  $Version = "latest"
}

if ([string]::IsNullOrWhiteSpace($InstallDir)) {
  $InstallDir = Join-Path $env:LOCALAPPDATA "zero\bin"
}

function Get-ZeroLatestTag {
  param([string]$Repository, [string]$GitHubApi)

  $apiBase = $GitHubApi.TrimEnd([char[]]"/")
  $release = Invoke-RestMethod `
    -Uri "$apiBase/repos/$Repository/releases/latest" `
    -Headers @{ Accept = "application/vnd.github+json" } `
    -TimeoutSec 15

  if ([string]::IsNullOrWhiteSpace($release.tag_name)) {
    throw "GitHub release response did not include tag_name"
  }

  return [string]$release.tag_name
}

function Get-ZeroArch {
  $arch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()

  switch ($arch) {
    "X64" { return "x64" }
    "Arm64" { return "arm64" }
    default { throw "Unsupported architecture: $arch" }
  }
}

function Find-ZeroExtractedFile {
  param(
    [string]$Root,
    [string]$FileName
  )

  $candidate = Join-Path $Root $FileName
  if (Test-Path $candidate -PathType Leaf) {
    return $candidate
  }

  $matches = @(Get-ChildItem -Path $Root -Filter $FileName -File -Recurse)
  if ($matches.Count -eq 1) {
    return $matches[0].FullName
  }

  throw "Release archive did not contain exactly one $FileName"
}

function Test-ZeroPathContainsDir {
  param(
    [string]$PathValue,
    [string]$Dir
  )

  if ([string]::IsNullOrEmpty($PathValue)) {
    return $false
  }

  return @($PathValue -split [System.IO.Path]::PathSeparator) -contains $Dir
}

function Find-ZeroOptionalExtractedDirectory {
  param(
    [string]$Root,
    [string]$DirectoryName
  )

  $candidate = Join-Path $Root $DirectoryName
  if (Test-Path $candidate -PathType Container) {
    return $candidate
  }

  $matches = @(
    Get-ChildItem -Path $Root -Directory | ForEach-Object {
      $child = Join-Path $_.FullName $DirectoryName
      if (Test-Path $child -PathType Container) {
        Get-Item $child
      }
    }
  )
  if ($matches.Count -eq 0) {
    return $null
  }
  if ($matches.Count -eq 1) {
    return $matches[0].FullName
  }

  throw "Release archive contained multiple $DirectoryName directories"
}

if ($Version -eq "latest") {
  $tag = Get-ZeroLatestTag -Repository $Repository -GitHubApi $GitHubApi
} elseif ($Version.StartsWith("v")) {
  $tag = $Version
} else {
  $tag = "v$Version"
}

$releaseVersion = $tag -replace "^v", ""
$arch = Get-ZeroArch
$archiveName = "zero-v$releaseVersion-windows-$arch.zip"
$checksumName = "$archiveName.sha256"
$releaseBase = $GitHubBaseUrl.TrimEnd([char[]]"/")
$releaseUrl = "$releaseBase/$Repository/releases/download/$tag"
$tempDir = Join-Path ([System.IO.Path]::GetTempPath()) ("zero-install-" + [System.Guid]::NewGuid().ToString("N"))
$extractDir = Join-Path $tempDir "extract"
$archivePath = Join-Path $tempDir $archiveName
$checksumPath = Join-Path $tempDir $checksumName

try {
  New-Item -ItemType Directory -Path $tempDir, $extractDir -Force | Out-Null

  Write-Host "Installing Zero $tag for windows-$arch"
  Invoke-WebRequest -Uri "$releaseUrl/$archiveName" -OutFile $archivePath -UseBasicParsing -TimeoutSec 300
  Invoke-WebRequest -Uri "$releaseUrl/$checksumName" -OutFile $checksumPath -UseBasicParsing -TimeoutSec 300

  $checksumLine = Get-Content -Path $checksumPath -TotalCount 1
  $expectedChecksum = ($checksumLine -split "\s+")[0].ToLowerInvariant()
  $actualChecksum = (Get-FileHash -Path $archivePath -Algorithm SHA256).Hash.ToLowerInvariant()

  if ($expectedChecksum -ne $actualChecksum) {
    throw "Checksum mismatch for $archiveName"
  }

  Expand-Archive -Path $archivePath -DestinationPath $extractDir -Force

  New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
  $requiredFiles = @(
    "zero.exe",
    "zero-windows-command-runner.exe",
    "zero-windows-sandbox-setup.exe"
  )
  foreach ($fileName in $requiredFiles) {
    $sourcePath = Find-ZeroExtractedFile -Root $extractDir -FileName $fileName
    Copy-Item -Path $sourcePath -Destination (Join-Path $InstallDir $fileName) -Force
  }
  $helpersPath = Find-ZeroOptionalExtractedDirectory -Root $extractDir -DirectoryName "helpers"
  if ($null -ne $helpersPath) {
    $targetHelpersPath = Join-Path $InstallDir "helpers"
    if (Test-Path $targetHelpersPath) {
      Remove-Item -Path $targetHelpersPath -Recurse -Force
    }
    Copy-Item -Path $helpersPath -Destination $targetHelpersPath -Recurse -Force
  }

  $targetPath = Join-Path $InstallDir "zero.exe"
  Write-Host "Installed $targetPath"

  $userPath = [Environment]::GetEnvironmentVariable("PATH", "User")
  if (-not (Test-ZeroPathContainsDir -PathValue $userPath -Dir $InstallDir)) {
    try {
      $newUserPath = if ([string]::IsNullOrEmpty($userPath)) { $InstallDir } else { "$userPath;$InstallDir" }
      [Environment]::SetEnvironmentVariable("PATH", $newUserPath, "User")
      Write-Host "Added $InstallDir to your user PATH. Restart your terminal to use 'zero'."
    } catch {
      Write-Warning "Could not update your user PATH automatically: $_"
      Write-Warning "Add $InstallDir to PATH manually to run zero from any directory."
    }
  }

  if (-not (Test-ZeroPathContainsDir -PathValue $env:PATH -Dir $InstallDir)) {
    $env:PATH = "$env:PATH;$InstallDir"
  }
} finally {
  if (Test-Path $tempDir) {
    Remove-Item -Path $tempDir -Recurse -Force
  }
}
