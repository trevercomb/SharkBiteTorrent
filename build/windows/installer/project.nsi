Unicode true

####
## Please note: Template replacements don't work in this file. They are provided with default defines like
## mentioned underneath.
## If the keyword is not defined, "wails_tools.nsh" will populate them with the values from ProjectInfo.
## If they are defined here, "wails_tools.nsh" will not touch them. This allows to use this project.nsi manually
## from outside of Wails for debugging and development of the installer.
##
## For development first make a wails nsis build to populate the "wails_tools.nsh":
## > wails build --target windows/amd64 --nsis
## Then you can call makensis on this file with specifying the path to your binary:
## For a AMD64 only installer:
## > makensis -DARG_WAILS_AMD64_BINARY=..\..\bin\app.exe
## For a ARM64 only installer:
## > makensis -DARG_WAILS_ARM64_BINARY=..\..\bin\app.exe
## For a installer with both architectures:
## > makensis -DARG_WAILS_AMD64_BINARY=..\..\bin\app-amd64.exe -DARG_WAILS_ARM64_BINARY=..\..\bin\app-arm64.exe
####
## The following information is taken from the ProjectInfo file, but they can be overwritten here.
####
## !define INFO_PROJECTNAME    "MyProject" # Default "{{.Name}}"
## !define INFO_COMPANYNAME    "MyCompany" # Default "{{.Info.CompanyName}}"
## !define INFO_PRODUCTNAME    "MyProduct" # Default "{{.Info.ProductName}}"
## !define INFO_PRODUCTVERSION "1.0.0"     # Default "{{.Info.ProductVersion}}"
## !define INFO_COPYRIGHT      "Copyright" # Default "{{.Info.Copyright}}"
###
## !define PRODUCT_EXECUTABLE  "Application.exe"      # Default "${INFO_PROJECTNAME}.exe"
## !define UNINST_KEY_NAME     "UninstKeyInRegistry"  # Default "${INFO_COMPANYNAME}${INFO_PRODUCTNAME}"
####
## !define REQUEST_EXECUTION_LEVEL "admin"            # Default "admin"  see also https://nsis.sourceforge.io/Docs/Chapter4.html
####
## Include the wails tools
####
!include "wails_tools.nsh"

# The version information for this two must consist of 4 parts, all numbers:
# a beta's "0.3.0-beta.1" is 0.3.0.0 here (the text fields below keep it).
!searchparse /noerrors "${INFO_PRODUCTVERSION}-" "" VERSION_NUMBERS "-"
VIProductVersion "${VERSION_NUMBERS}.0"
VIFileVersion    "${VERSION_NUMBERS}.0"

VIAddVersionKey "CompanyName"     "${INFO_COMPANYNAME}"
VIAddVersionKey "FileDescription" "${INFO_PRODUCTNAME} Installer"
VIAddVersionKey "ProductVersion"  "${INFO_PRODUCTVERSION}"
VIAddVersionKey "FileVersion"     "${INFO_PRODUCTVERSION}"
VIAddVersionKey "LegalCopyright"  "${INFO_COPYRIGHT}"
VIAddVersionKey "ProductName"     "${INFO_PRODUCTNAME}"

# Enable HiDPI support. https://nsis.sourceforge.io/Reference/ManifestDPIAware
ManifestDPIAware true

!include "MUI.nsh"

!define MUI_ICON "..\icon.ico"
!define MUI_UNICON "..\icon.ico"
# !define MUI_WELCOMEFINISHPAGE_BITMAP "resources\leftimage.bmp" #Include this to add a bitmap on the left side of the Welcome Page. Must be a size of 164x314
!define MUI_FINISHPAGE_NOAUTOCLOSE # Wait on the INSTFILES page so the user can take a look into the details of the installation steps
!define MUI_ABORTWARNING # This will warn the user if they exit from the installer.

!insertmacro MUI_PAGE_WELCOME # Welcome to the installer page.
# !insertmacro MUI_PAGE_LICENSE "resources\eula.txt" # Adds a EULA page to the installer
!insertmacro MUI_PAGE_DIRECTORY # In which folder install page.
!insertmacro MUI_PAGE_INSTFILES # Installing page.
!insertmacro MUI_PAGE_FINISH # Finished installation page.

!insertmacro MUI_UNPAGE_INSTFILES # Uinstalling page

!insertmacro MUI_LANGUAGE "English" # Set the Language of the installer

## The following two statements can be used to sign the installer and the uninstaller. The path to the binaries are provided in %1
#!uninstfinalize 'signtool --file "%1"'
#!finalize 'signtool --file "%1"'

Name "${INFO_PRODUCTNAME}"
OutFile "..\..\bin\${INFO_PROJECTNAME}-${ARCH}-installer.exe" # Name of the installer's file.
!ifdef WAILS_INSTALL_SCOPE
  !if "${WAILS_INSTALL_SCOPE}" == "user"
    InstallDir "$LOCALAPPDATA\Programs\${INFO_PRODUCTNAME}"
  !else
    InstallDir "$PROGRAMFILES64\${INFO_COMPANYNAME}\${INFO_PRODUCTNAME}"
  !endif
!else
  InstallDir "$PROGRAMFILES64\${INFO_COMPANYNAME}\${INFO_PRODUCTNAME}"
!endif # Default installing folder ($PROGRAMFILES is Program Files folder).
# Reinstalling or updating goes back to wherever it was installed (the
# in-app updater also passes /D=<the app's folder>).
InstallDirRegKey HKCU "${UNINST_KEY}" "InstallLocation"
ShowInstDetails show # This will always show the installation details.

# closeApp waits up to 20 s for this user's running copy to exit, then
# forces it. Only processes in the installer's own session (this user).
!define PS '"$SYSDIR\WindowsPowerShell\v1.0\powershell.exe" -NoProfile -NonInteractive -Command'
!define MINE 'Get-Process -Name SharkBiteTorrent -ErrorAction SilentlyContinue | Where-Object { $$_.SessionId -eq (Get-Process -Id $$PID).SessionId }'
!macro closeApp
    nsExec::Exec '${PS} "${MINE} | Wait-Process -Timeout 20 -ErrorAction SilentlyContinue"'
    !insertmacro killApp
!macroend
!macro killApp
    nsExec::Exec '${PS} "${MINE} | Stop-Process -Force -ErrorAction SilentlyContinue"'
!macroend

Function .onInit
   !insertmacro wails.checkArchitecture
FunctionEnd

Section
    !insertmacro wails.setShellContext

    !insertmacro wails.webview2runtime

    # Close a running copy so its exe can be replaced (upgrade install), and
    # wait until it has really exited: the app takes a moment to close its
    # files and progress database, and copying over a still-running exe fails.
    # In silent mode (used by the in-app updater) that failure used to be
    # skipped without any message, leaving the old version installed.
    # The updater has already asked the app to quit, so wait for that first
    # and only force it after. PowerShell by full path: a bare program name
    # would also be looked for in the installer's own folder (Downloads).
    !insertmacro closeApp

    SetOutPath $INSTDIR

    # Copy the app, retrying while Windows still has the old exe locked.
    SetOverwrite try
    StrCpy $R9 0
    copy_files:
    ClearErrors
    !insertmacro wails.files
    IfErrors 0 files_copied
    IntOp $R9 $R9 + 1
    IntCmp $R9 10 files_failed
    DetailPrint "Shark BiteTorrent is still closing, retrying..."
    !insertmacro killApp
    Sleep 1000
    Goto copy_files
    files_failed:
    SetErrorLevel 2
    MessageBox MB_ICONSTOP "Couldn't replace ${PRODUCT_EXECUTABLE} because it's still running. Close Shark BiteTorrent (including from the tray) and run the installer again." /SD IDOK
    Abort
    files_copied:
    SetOverwrite on
    # License texts ship with the app (GPL-3.0-or-later + third-party notices).
    File "..\..\..\LICENSE"
    File "..\..\..\THIRD_PARTY_LICENSES.txt"

    CreateShortcut "$SMPROGRAMS\${INFO_PRODUCTNAME}.lnk" "$INSTDIR\${PRODUCT_EXECUTABLE}"
    CreateShortCut "$DESKTOP\${INFO_PRODUCTNAME}.lnk" "$INSTDIR\${PRODUCT_EXECUTABLE}"

    # Register as the handler for magnet: links and .torrent files (HKCU).
    # Uses the app's own code so it matches the button in Settings.
    DetailPrint "Registering magnet links and .torrent files..."
    ExecWait '"$INSTDIR\${PRODUCT_EXECUTABLE}" --register-associations'

    !insertmacro wails.writeUninstaller
    WriteRegStr HKCU "${UNINST_KEY}" "InstallLocation" "$INSTDIR"
SectionEnd

Function .onInstSuccess
    # Offer to launch from the finish page is noisy; just start it.
    Exec '"$INSTDIR\${PRODUCT_EXECUTABLE}"'
FunctionEnd

Section "uninstall"
    !insertmacro wails.setShellContext

    !insertmacro closeApp

    # Remove magnet/.torrent registrations that still point at this app.
    ExecWait '"$INSTDIR\${PRODUCT_EXECUTABLE}" --unregister-associations'
    # And the "start when I sign in" entry, if it was turned on.
    ExecWait '"$INSTDIR\${PRODUCT_EXECUTABLE}" --remove-startup'
    # The app ID its notifications are shown under (notify_windows.go).
    DeleteRegKey HKCU "Software\Classes\AppUserModelId\SharkBiteTorrent"

    RMDir /r "$AppData\${PRODUCT_EXECUTABLE}" # Remove the WebView2 DataPath

    # Delete only what the installer put there. The install folder can be
    # changed during setup, so a recursive delete of $INSTDIR could wipe a
    # folder full of the user's own files (e.g. if installed into Downloads).
    Delete "$INSTDIR\${PRODUCT_EXECUTABLE}"
    Delete "$INSTDIR\LICENSE"
    Delete "$INSTDIR\THIRD_PARTY_LICENSES.txt"

    Delete "$SMPROGRAMS\${INFO_PRODUCTNAME}.lnk"
    Delete "$DESKTOP\${INFO_PRODUCTNAME}.lnk"

    # Settings, the WARP account (%APPDATA%\SharkBiteTorrent) and downloads are kept.

    !insertmacro wails.deleteUninstaller
    RMDir $INSTDIR # only if it's now empty
SectionEnd
