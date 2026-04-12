@echo off
setlocal
cd /d %~dp0
set "BASE_URL=http://127.0.0.1:18080"
set "HEALTH_URL=%BASE_URL%/healthz"
set "SETTINGS_URL=%BASE_URL%/ui/telegram-settings.html"
set "COMMAND_URL=%BASE_URL%/telegram/command"
set "STATE_DIR=%~dp0state"
set "HINT_FILE=%STATE_DIR%\boot_info.txt"

if not exist "%STATE_DIR%" mkdir "%STATE_DIR%"
if not exist "%~dp0mantisx_server.exe" (
  echo mantisx_server.exe not found
  exit /b 1
)

> "%HINT_FILE%" (
  echo MantisX Quick Access
  echo ========================================
  echo Health URL: %HEALTH_URL%
  echo Settings UI: %SETTINGS_URL%
  echo Command API: %COMMAND_URL%
  echo.
  echo If Telegram bot does not respond:
  echo 1. Open Settings UI
  echo 2. Set Telegram Bot Token
  echo 3. Set Allowed Users ^(chat/user ID^)
  echo 4. Save, then send /start in Telegram
)

start "" "%~dp0mantisx_server.exe"
timeout /t 2 /nobreak >nul
powershell -NoProfile -Command "try { (Invoke-WebRequest -UseBasicParsing %HEALTH_URL%).Content } catch { Write-Output 'healthz failed' }"
start "" "%SETTINGS_URL%"
echo.
echo [MantisX] Health     : %HEALTH_URL%
echo [MantisX] Settings   : %SETTINGS_URL%
echo [MantisX] Quick File : %HINT_FILE%
echo.
echo Keep this window open while operating MantisX.
pause
endlocal
