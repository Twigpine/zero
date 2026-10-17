param(
    [Parameter(Mandatory)][string]$NsisDir,
    [Parameter(Mandatory)][string]$StagingDir,
    [Parameter(Mandatory)][string]$OutputDir,
    [Parameter(Mandatory)][string]$Version,
    [Parameter(Mandatory)][string]$Arch
)

$ErrorActionPreference = "Stop"
$pkgName = "zero-v${Version}-windows-${Arch}"
$installerName = "${pkgName}-installer.exe"
$installerPath = Join-Path $OutputDir $installerName
$nsiPath = Join-Path $env:TEMP "zero-installer.nsi"

@"
!include "MUI2.nsh"

Name "Zero v${Version}"
OutFile "$installerPath"
InstallDir "$PROGRAMFILES64\Zero"
RequestExecutionLevel admin
BrandingText "Zero Installer"

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH

!insertmacro MUI_LANGUAGE "English"

Section "Install"
  SetOutPath "`$INSTDIR"
  File /r "${StagingDir}\*.*"
  WriteUninstaller "`$INSTDIR\uninstall.exe"

  ReadRegStr `$R0 HKCU "Environment" "Path"
  StrCmp `$R0 "" addPath
    StrCpy `$R0 "`$R0;`$INSTDIR"
    Goto writePath
  addPath:
    StrCpy `$R0 "`$INSTDIR"
  writePath:
    WriteRegStr HKCU "Environment" "Path" "`$R0"
    System::Call "user32::SendMessage(i 0xffff, i 0x1a, i 0, i 0) i"

  CreateDirectory "`$SMPROGRAMS\Zero"
  CreateShortCut "`$SMPROGRAMS\Zero\Zero.lnk" "`$INSTDIR\zero.exe" "" "`$INSTDIR\zero.exe" 0
  CreateShortCut "`$SMPROGRAMS\Zero\Uninstall Zero.lnk" "`$INSTDIR\uninstall.exe"

  CreateShortCut "`$DESKTOP\Zero.lnk" "`$INSTDIR\zero.exe" "" "`$INSTDIR\zero.exe" 0

  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\Zero" \
    "DisplayName" "Zero v${Version}"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\Zero" \
    "UninstallString" "`"`$INSTDIR\uninstall.exe`""
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\Zero" \
    "DisplayVersion" "${Version}"
  WriteRegDWORD HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\Zero" \
    "NoModify" 1
  WriteRegDWORD HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\Zero" \
    "NoRepair" 1
SectionEnd

Section "Uninstall"
  ReadRegStr `$R0 HKCU "Environment" "Path"
  Push `$R0
  Push "`$INSTDIR"
  Call un.StrDel
  Pop `$R0
  WriteRegStr HKCU "Environment" "Path" "`$R0"
  System::Call "user32::SendMessage(i 0xffff, i 0x1a, i 0, i 0) i"

  RmDir /r "`$SMPROGRAMS\Zero"
  Delete "`$DESKTOP\Zero.lnk"

  DeleteRegKey HKLM "Software\Microsoft\Windows\CurrentVersion\Uninstall\Zero"

  RmDir /r "`$INSTDIR"
SectionEnd

Function un.StrDel
  Exch `$R1
  Exch
  Exch `$R0
  Push `$R2
  Push `$R3
  Push `$R4
  StrCpy `$R2 `$R0
  StrLen `$R3 `$R1
  loop:
    StrCpy `$R4 `$R2 `$R3
    StrCmp `$R4 `$R1 0 next
    StrCpy `$R4 `$R2 "" `$R3
    StrLen `$R5 `$R2
    IntOp `$R5 `$R5 - `$R3
    StrCpy `$R2 `$R2 `$R5 `$R3
    Goto done
  next:
    StrCpy `$R4 `$R2 1
    StrCmp `$R4 "" done 0
    StrCpy `$R2 `$R2 "" 1
    Goto loop
  done:
    StrCpy `$R0 `$R2
    Pop `$R4
    Pop `$R3
    Pop `$R2
    Pop `$R1
    Exch `$R0
FunctionEnd
"@ | Set-Content -Path $nsiPath -Encoding ASCII

New-Item -ItemType Directory -Force -Path $OutputDir | Out-Null

$makensis = Join-Path $NsisDir "makensis.exe"
Write-Host "Compiling $installerName ..."
& $makensis $nsiPath
if ($LASTEXITCODE -ne 0) { throw "makensis failed" }

$hash = (Get-FileHash $installerPath -Algorithm SHA256).Hash.ToLower()
"$hash  $installerName" | Set-Content "${installerPath}.sha256" -Encoding ASCII

Write-Host "Created $installerPath"
