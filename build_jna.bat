@echo off
cd /d "%~dp0"

set PATH=C:\zig-windows-x86_64-0.14.0;%PATH%

echo === Building frpc_jna.dll (Windows x86_64) ===
set "CC=zig cc -target x86_64-windows-gnu"
set "CXX=zig c++ -target x86_64-windows-gnu"
set CGO_ENABLED=1
set GOOS=windows
set GOARCH=amd64
go build -buildmode=c-shared -ldflags="-s -w" -o bin\frpc_jna.dll .\jna\ 2>&1
if %ERRORLEVEL% NEQ 0 (
    echo DLL build failed!
    exit /b 1
)
echo DLL build success!

echo.
echo === Building frpc_jna.so (Linux x86_64) ===
set "CC=zig cc -target x86_64-linux-gnu"
set "CXX=zig c++ -target x86_64-linux-gnu"
set CGO_ENABLED=1
set GOOS=linux
set GOARCH=amd64
go build -buildmode=c-shared -ldflags="-s -w" -o bin\frpc_jna.so .\jna\ 2>&1
if %ERRORLEVEL% NEQ 0 (
    echo SO build failed!
    exit /b 1
)
echo SO build success!

echo.
echo === Build complete! ===
dir bin\frpc_jna.*
