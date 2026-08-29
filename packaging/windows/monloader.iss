#define MyAppName "Monloader"
#define MyAppExeName "monloader.exe"
#define MyAppPublisher "monbooru"
#define MyAppURL "https://github.com/monbooru/monloader"

#ifndef MyAppVersion
  #define MyAppVersion "0.0.0"
#endif

#ifndef MyAppTools
  #define MyAppTools "bundled"
#endif

[Setup]
AppId={{B2F3BE18-1C24-5C94-8D9C-1F09E7061D2B}
AppName={#MyAppName}
AppVersion={#MyAppVersion}
AppPublisher={#MyAppPublisher}
AppPublisherURL={#MyAppURL}
AppSupportURL={#MyAppURL}/issues
DefaultDirName={autopf}\{#MyAppName}
DefaultGroupName={#MyAppName}
DisableProgramGroupPage=yes
UninstallDisplayIcon={app}\{#MyAppExeName}
OutputDir=..\..\dist
OutputBaseFilename=monloader_{#MyAppVersion}_desktop_{#MyAppTools}_windows_amd64_setup
SetupIconFile=..\icons\monloader.ico
Compression=lzma2
SolidCompression=yes
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
PrivilegesRequired=lowest
WizardStyle=modern

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"

[Files]
Source: "payload\*"; DestDir: "{app}"; Flags: ignoreversion recursesubdirs createallsubdirs
Source: "..\icons\monloader.ico"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
Name: "{group}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"; Parameters: "-desktop"; IconFilename: "{app}\monloader.ico"
Name: "{autodesktop}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"; Parameters: "-desktop"; IconFilename: "{app}\monloader.ico"; Tasks: desktopicon

[Run]
Filename: "{app}\{#MyAppExeName}"; Parameters: "-desktop"; Description: "{cm:LaunchProgram,{#StringChange(MyAppName, '&', '&&')}}"; Flags: nowait postinstall skipifsilent

[UninstallRun]
Filename: "{sys}\taskkill.exe"; Parameters: "/f /im {#MyAppExeName}"; Flags: runhidden; RunOnceId: "StopApp"

[UninstallDelete]
Type: files; Name: "{app}\monloader.ico"

[Registry]
Root: HKCU; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; ValueName: "monloader"; Flags: dontcreatekey uninsdeletevalue
