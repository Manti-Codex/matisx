# 데몬 + Codex 운영 요약

`C:\mantisx` 기준의 빠른 운영 메모입니다.
상세 절차는 `docs/daemon_server_ops.md`를 사용하세요.

## 기본 원칙
- 기본 백엔드: `appserver`
- 필요 시만 `rpc` 모드 사용
- 서버 엔트리: `cmd/mantisx_server/main.go`

## 실행
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

RPC:
```powershell
cd C:\mantisx
cmd /c .\run_mantisx_rpc_stack.bat
```

## 필수 점검
```powershell
Invoke-WebRequest http://127.0.0.1:18080/healthz
Invoke-RestMethod http://127.0.0.1:18080/api/settings/telegram
```

UI:
- `http://127.0.0.1:18080/ui/telegram-settings.html`

## 운영 명령
- `/help`, `/start`
- `/clear`, `/stop_mcp`
- `/approvals`, `/approval_mode`
- `/approval_on`, `/approval on`, `/approval_off`, `/approval off`
- `/approve [id]`, `/deny [id]`
- `1|2|3|4` 빠른 승인/거부

## 핵심 환경변수
- `MANTISX_BACKEND_MODE` (기본 `appserver`)
- `MANTISX_HTTP_ADDR` (기본 `:18080`)
- `MANTISX_COMMAND_TIMEOUT_SEC` (기본 `240`)
- `MANTISX_APPROVAL_ENABLED` (AppServer 기본 `true`, RPC 기본 `false`)

RPC 전용:
- `MANTISX_CODEX_DAEMON_ADDR` (기본 `127.0.0.1:17997`)
- `MANTISX_CODEX_IDLE_MS` (기본 `20000`)
- `MANTISX_CODEX_MAX_MS` (기본 `150000`)

## 참고
- 상세 운영/장애 대응: `docs/daemon_server_ops.md`
- 작업/수정 기준: `agent.md`
