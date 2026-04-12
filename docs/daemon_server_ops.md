# Mantisx 운영 가이드 (AppServer/RPC)

이 문서는 `C:\mantisx` 기준 운영 절차를 정리합니다.
현재 코드 기본 백엔드는 `appserver`이며, 필요 시 `rpc`로 전환할 수 있습니다.

## 1. 구성 요약
- 서버 엔트리: `C:\mantisx\cmd\mantisx_server\main.go`
- 서버 바이너리: `C:\mantisx\cmd\bin\mantisx_server.exe`
- 설정 파일: `C:\mantisx\state\telegram_settings.json`
- HTTP 기본 주소: `:18080`

요청 흐름:
- AppServer: `Telegram -> mantisx_server -> codex app-server(stdio)`
- RPC: `Telegram -> mantisx_server -> codex_daemon_probe(RPC) -> codex mcp-server`

## 2. 빠른 시작
기본(AppServer):
```powershell
cd C:\mantisx
cmd /c .\run_mantisx_stack.bat
```

컴파일 후 안전 재실행(AppServer):
```powershell
cd C:\mantisx
cmd /c .\rebuild_restart_mantisx_stack.bat
```

명시적 AppServer:
```powershell
cd C:\mantisx
cmd /c .\run_mantisx_appserver_stack.bat
```

RPC 레거시:
```powershell
cd C:\mantisx
cmd /c .\run_mantisx_rpc_stack.bat
```

## 3. 스크립트 동작
`run_mantisx_stack.bat` / `run_mantisx_appserver_stack.bat`:
1. 기존 `mantisx_server.exe` 종료
2. 서버를 `MANTISX_BACKEND_MODE=appserver`로 시작
3. `:18080` 포트 확인

`rebuild_restart_mantisx_stack.bat`:
1. `go build -o .\cmd\bin\mantisx_server.exe .\cmd\mantisx_server` 실행
2. 빌드 성공 시에만 기존 `mantisx_server.exe` 종료
3. 2초 대기 후 AppServer 모드로 서버 시작
4. `:18080` 포트/헬스체크 확인

`run_mantisx_rpc_stack.bat`:
1. 기존 `codex_daemon_probe.exe`, `mantisx_server.exe` 종료
2. RPC 데몬 시작 (`127.0.0.1:17997`)
3. 서버를 `MANTISX_BACKEND_MODE=rpc`로 시작
4. `:17997`, `:18080` 포트 확인

## 4. 빌드 및 점검
빌드:
```powershell
cd C:\mantisx
go build -o .\cmd\bin\mantisx_server.exe .\cmd\mantisx_server
```

헬스체크:
```powershell
Invoke-WebRequest http://127.0.0.1:18080/healthz
```
정상값: `HTTP 200`, body `ok`

설정 조회:
```powershell
Invoke-RestMethod http://127.0.0.1:18080/api/settings/telegram
```

운영 UI:
- `http://127.0.0.1:18080/ui/telegram-settings.html`
- `/` 접근 시 UI로 리다이렉트

## 5. Telegram 운영 명령
- `/help`, `/start`
- `/clear`
- `/stop_mcp`
- `/approvals`
- `/approval_mode`
- `/approval_on`, `/approval on`
- `/approval_off`, `/approval off`
- `/approve [id]`, `/deny [id]`
- `1`, `2`, `3`, `4` (빠른 승인/거부)
- 기타 텍스트: Codex 질의 전달

## 6. 설정 파일 스키마
`state/telegram_settings.json`:
```json
{
  "token": "<telegram bot token>",
  "allowed_users": "123456789,987654321",
  "workspace_dir": "C:\\mantisx",
  "codex_daemon_addr": "127.0.0.1:17997",
  "updated_at": "2026-04-12 01:23:45"
}
```

참고:
- `allowed_users`는 콤마 구분 숫자 문자열
- `workspace_dir` 설정 시 workspace policy 프롬프트 자동 주입
- `codex_daemon_addr`는 RPC 모드에서 사용, AppServer 모드에서는 사실상 무시

## 7. 주요 환경변수 (코드 기본값)
서버:
- `MANTISX_HTTP_ADDR` 기본 `:18080`
- `MANTISX_SETTINGS_FILE` 기본 `<workdir>\\state\\telegram_settings.json`
- `MANTISX_COMMAND_TIMEOUT_SEC` 기본 `240`

백엔드 선택:
- `MANTISX_BACKEND_MODE`:
  - `appserver|app-server|app_server|""` -> AppServer
  - 그 외 -> RPC

공통:
- `MANTISX_APPROVAL_ENABLED`:
  - AppServer 기본 `true`
  - RPC 기본 `false`
- `MANTISX_WORKSPACE_DIR` (workspace policy 주입)

AppServer:
- `MANTISX_CODEX_EXE` 기본 `codex`
- `MANTISX_CODEX_CWD` 선택
- `MANTISX_APP_SERVER_LOG` 기본 `<workdir>\\state\\app_server_controller.log`

RPC:
- `MANTISX_CODEX_DAEMON_ADDR` 기본 `127.0.0.1:17997`
- `BACKTESTER_CODEX_DAEMON_ADDR` fallback
- `MANTISX_CODEX_IDLE_MS` 기본 `20000`, 최소 `4000`
- `MANTISX_CODEX_MAX_MS` 기본 `150000`
- `MANTISX_MCP_INIT_IDLE_MS` 기본 `4000` (내부 보정)
- `MANTISX_MCP_INIT_MAX_MS` 기본 `240000` (내부 보정)

Telegram poller:
- `MANTISX_TG_POLL_TIMEOUT_SEC` 기본 `30` (10~50 보정)
- `MANTISX_TG_PROGRESS_SEC` 기본 `4` (최소 2)
- `MANTISX_TG_CHUNK_CHARS` 기본 `1200` (300~3500 보정)
- `MANTISX_TG_HARD_TIMEOUT_SEC` 기본 `0` (자동 계산)
- `MANTISX_TG_WRITE_TIMEOUT_SEC` 기본 `8` (3~20 보정)

## 8. 장애 대응
`:18080` 미리스닝/헬스체크 실패:
1. `netstat -ano | findstr :18080`
2. 서버 재기동: `cmd /c C:\mantisx\run_mantisx_stack.bat`

RPC 모드에서 `mcp initialize: context deadline exceeded`:
1. `netstat -ano | findstr :17997`
2. `codex_daemon_probe.exe`/`mantisx_server.exe` 재기동
3. 반복 시 `MANTISX_MCP_INIT_MAX_MS`, `MANTISX_COMMAND_TIMEOUT_SEC` 조정

Telegram 응답 지연:
1. `state\telegram_settings.json`의 token/allowed_users/workspace_dir 확인
2. approval 대기 상태(`/approvals`) 확인
3. poller timeout 관련 환경변수 조정 후 재기동

## 9. 일일 체크리스트
- `GET /healthz` 정상 확인
- `:18080` 리슨 확인 (RPC 모드면 `:17997`도 확인)
- Telegram `/help` 응답 확인
- 일반 질의 1건 응답 확인
- 승인 모드 사용 시 `/approval_mode`, `/approvals` 확인
