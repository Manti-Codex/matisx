@echo off
setlocal
cd /d %~dp0
if not exist "%~dp0mantisx_server.exe" (
  echo mantisx_server.exe not found
  exit /b 1
)
start "" /b "%~dp0mantisx_server.exe"
timeout /t 2 /nobreak >nul
powershell -NoProfile -Command "try { (Invoke-WebRequest -UseBasicParsing http://127.0.0.1:18080/healthz).Content } catch { Write-Output 'healthz failed' }"
endlocal
