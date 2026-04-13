# MantisX - Codex Telegram Remote Controller (Binary Distribution)

MantisX is a Codex Telegram remote controller for Windows.
If users search for `codex telegram`, `telegram codex`, or `codex remote control`, this project is intended to match that use case.
This repository is distribution-only and keeps runtime artifacts, not source code.

## Keywords
- codex telegram
- telegram codex remote control
- codex remote controller for windows

## What This Is
- Telegram bot interface for Codex
- Remote Codex control from mobile Telegram chat
- Approval flow support (accept, accept for session, decline, cancel)
- Approval helper keyboard: when approval is pending, Telegram `1/2/3/4` buttons are sent automatically
- Session memory with restart recovery

## Included Files
- `mantisx_server.exe`
- `start_mantisx.bat`
- `README.md`
- `tools/`

## Quick Start (Windows)
1. Install Codex CLI:
```powershell
npm i -g @openai/codex
```
2. Login:
```powershell
codex login
```
3. Start MantisX:
```powershell
.\start_mantisx.bat
```
4. Open Settings UI:
- `http://127.0.0.1:18081/ui/telegram-settings.html`

## Telegram Settings
In the UI, configure:
- `Telegram Bot Token`
- `Allowed Users` (comma-separated chat/user IDs)
- `Workspace Dir`
- `Codex Daemon Addr` (if RPC backend is used)
- `Language` (`ko` or `en`)

For dual-workspace operation:
- Add extra workdir(s) and apply:
```powershell
powershell -ExecutionPolicy Bypass -File .\tools\workdir_manager.ps1 add "C:\your\extra\dir"
```
- Remove:
```powershell
powershell -ExecutionPolicy Bypass -File .\tools\workdir_manager.ps1 remove "C:\your\extra\dir"
```
- List:
```powershell
powershell -ExecutionPolicy Bypass -File .\tools\workdir_manager.ps1 list
```
- `workspace_dir` is automatically set to `.\state\workspace_root` and includes:
  - `mantisx-root` -> `c:\mantisx`
  - one junction per extra workdir

Language setting controls Telegram-facing messages and progress text.

## Memory System (3 Layers)
MantisX stores memory per session:
- Long-term memory: explicit facts/preferences (`/remember`)
- Mid-term memory: rolling turn summaries
- Temporary memory: recent conversation buffer

Runtime file:
- `state/memory_layers.json`

Behavior:
- Auto-loaded on server restart
- Session-scoped isolation
- Can be inspected with `/memory`
- Can be cleared with `/memory_clear`

## Telegram Commands
- `/start`, `/help`
- `/clear`, `/stop_mcp`
- `/approval on`, `/approval off`, `/approval_mode`
- `/approvals`, `/approve [id]`, `/deny [id]`
- Quick approval: `1`, `2`, `3`, `4`
- You can use either keyboard buttons or manual number input (`1/2/3/4`)
- Workdir helper (execute via Codex):
  - `/add_workdir <abs_path>` -> run `tools/workdir_manager.ps1 add "<abs_path>"`
  - `/remove_workdir <abs_path>` -> run `tools/workdir_manager.ps1 remove "<abs_path>"`
  - `/list_workdir` -> run `tools/workdir_manager.ps1 list`
- `/memory`, `/remember <text>`, `/remember_cancel`, `/memory_clear`

## Troubleshooting
- Health check:
  - `http://127.0.0.1:18081/healthz`
- If Codex login is missing:
  - run `codex login`
- If bot does not respond:
  - verify token + allowed user ID in Settings UI
  - save settings and send `/start` again

## Git Push Policy (Required)
Only push:
- `mantisx_server.exe`
- `README.md`
- `tools/` (all children)

Do not push:
- source folders (`cmd/`, `internal/`, `docs/`, etc.)
- local source staging folder (`src/`)
- runtime data (`state/`)
- temporary folders (`tmp_*`)
