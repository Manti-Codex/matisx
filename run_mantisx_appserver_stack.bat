@echo off
setlocal

set ROOT=C:\mantisx
set STATE=%ROOT%\state
set SERVER_EXE=%ROOT%\cmd\bin\mantisx_server.exe
set CODEX_EXE=C:\Users\stock\AppData\Roaming\npm\node_modules\@openai\codex\node_modules\@openai\codex-win32-x64\vendor\x86_64-pc-windows-msvc\codex\codex.exe

if not exist "%STATE%" mkdir "%STATE%"

echo [1/5] stop old server...
taskkill /F /IM mantisx_server.exe /T >nul 2>nul

echo [2/5] start mantisx server (backend=appserver)...
powershell -NoProfile -Command "$env:MANTISX_BACKEND_MODE='appserver'; $env:MANTISX_CODEX_EXE='%CODEX_EXE%'; $env:MANTISX_APPROVAL_ENABLED='true'; Start-Process -FilePath '%SERVER_EXE%' -WorkingDirectory '%ROOT%'"

timeout /t 2 /nobreak >nul

echo [3/5] port check...
netstat -ano | findstr :18080

echo [4/5] health check...
powershell -NoProfile -Command "try { $r=Invoke-WebRequest -UseBasicParsing http://127.0.0.1:18080/healthz -TimeoutSec 5; Write-Host ('healthz=' + $r.Content) } catch { Write-Host ('healthz failed: ' + $_.Exception.Message) }"

echo [5/5] memory restore probe...
powershell -NoProfile -Command "$settingsPath = Join-Path '%STATE%' 'telegram_settings.json'; if (!(Test-Path $settingsPath)) { Write-Host '[memory] skipped: settings file missing'; exit 0 }; $cfg = Get-Content -Raw $settingsPath | ConvertFrom-Json; $first = (($cfg.allowed_users -split '[,;\s]') | Where-Object { $_ -match '^\d+$' } | Select-Object -First 1); if (-not $first) { Write-Host '[memory] skipped: allowed_users empty'; exit 0 }; $session = 'tg:chat:' + $first; try { $body = @{ text='/memory'; session_key=$session; request_id=('boot-' + [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()) } | ConvertTo-Json -Compress; $res = Invoke-RestMethod -Method Post -Uri 'http://127.0.0.1:18080/telegram/command' -ContentType 'application/json' -Body $body -TimeoutSec 20; if ($res -and $res.message) { Write-Host '[memory]'; Write-Host $res.message } else { Write-Host '[memory] probe ok (empty message)' } } catch { Write-Host ('[memory] probe failed: ' + $_.Exception.Message) }"

echo done.
endlocal
