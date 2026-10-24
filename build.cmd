@echo off
rem Builds dist\Raspak.exe: icons (program and archive files), manifest and version in resources, no console window.
rem Pure Go (CGO_ENABLED=0), no C compiler needed. Version is taken from the VERSION file.
setlocal
cd /d "%~dp0"
set /p VERSION=<VERSION
go run github.com/tc-hib/go-winres@v0.3.3 make --in winres\winres.json --arch amd64 ^
  --product-version %VERSION% --file-version %VERSION% --out rsrc || exit /b 1
set CGO_ENABLED=0
go build -trimpath -ldflags "-H=windowsgui -s -w -X main.appVersion=%VERSION%" -o dist\Raspak.exe . || exit /b 1
echo Built: dist\Raspak.exe (%VERSION%)
