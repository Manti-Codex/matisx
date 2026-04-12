@echo off
setlocal

set ROOT=C:\mantisx
if not exist "%ROOT%\rebuild_restart_mantisx_stack.bat" (
  echo rebuild script not found: %ROOT%\rebuild_restart_mantisx_stack.bat
  exit /b 1
)

rem Delay to let current Telegram response flush before self-restart.
timeout /t 3 /nobreak >nul
cmd /c "%ROOT%\rebuild_restart_mantisx_stack.bat"

endlocal
