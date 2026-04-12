@echo off
setlocal

set ROOT=C:\mantisx
set STATE=%ROOT%\state
set CODEX_EXE=C:\Users\stock\AppData\Roaming\npm\node_modules\@openai\codex\node_modules\@openai\codex-win32-x64\vendor\x86_64-pc-windows-msvc\codex\codex.exe
set SERVER_EXE=%ROOT%\cmd\bin\mantisx_server.exe

if not exist "%STATE%" mkdir "%STATE%"

echo [1/6] build mantisx server...
pushd "%ROOT%"
go build -o .\cmd\bin\mantisx_server.exe .\cmd\mantisx_server
if errorlevel 1 (
  popd
  echo build failed. keep current processes.
  exit /b 1
)
popd

echo [2/6] stop old server...
taskkill /F /IM mantisx_server.exe /T >nul 2>nul

echo [3/6] wait 2 sec before restart...
timeout /t 2 /nobreak >nul

echo [4/6] start mantisx server (backend=appserver)...
powershell -NoProfile -Command "$env:MANTISX_BACKEND_MODE='appserver'; $env:MANTISX_CODEX_EXE='%CODEX_EXE%'; $env:MANTISX_APPROVAL_ENABLED='true'; Start-Process -FilePath '%SERVER_EXE%' -WorkingDirectory '%ROOT%'"

timeout /t 2 /nobreak >nul

echo [5/6] ports check...
netstat -ano | findstr :18080

echo [6/6] health check...
powershell -NoProfile -Command "try { $r=Invoke-WebRequest -UseBasicParsing http://127.0.0.1:18080/healthz -TimeoutSec 5; Write-Host ('healthz=' + $r.Content) } catch { Write-Host ('healthz failed: ' + $_.Exception.Message) }"

echo done.
endlocal
