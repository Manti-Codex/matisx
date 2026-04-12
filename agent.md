# Mantisx Agent Guide

이 문서는 `C:\mantisx`에서 작업하는 에이전트용 최신 운영 기준입니다.
기본 백엔드는 App Server이며, `MANTISX_FAILOVER_ENABLED=true`일 때 failover 래퍼가 활성화됩니다.

## 1) 현재 구조 (코드 기준)
- 진입점: `cmd/mantisx_server/main.go`
- 컨트롤러 팩토리: `internal/mcp/factory.go`
- 기본 컨트롤러: `AppServerController` (`internal/mcp/app_server_controller.go`)
- 대체 컨트롤러: `RPCController` (`internal/mcp/rpc_controller.go`)
- 선택 래퍼: `FailoverController` (`internal/mcp/failover_controller.go`)
- 공통 계약: `internal/mcp/controller.go`
  - `Controller`
  - `WorkspaceSetter`
  - `ProgressReporter`

요청 흐름:
- `Telegram -> poller -> service -> mcp.Controller -> app-server/rpc`
- 진행상태는 `ProgressReporter.GetProgress`를 poller가 읽어 상태 메시지에 반영

## 2) 세션/요청 계약
- HTTP `POST /telegram/command`는 `session_key`가 필수
- `session_key`별로 thread/approval 상태를 분리 관리
- `request_id`가 없으면 서버가 자동 생성
- timeout 후 늦게 도착한 goroutine 결과는 stale로 버려짐

관련 코드:
- `cmd/mantisx_server/main.go` (`activeReqBySession`)
- `internal/mcp/session_context.go` (`WithSessionKey`, `SessionKeyFromContext`)
- `internal/mcp/session_store.go` (RPC 세션 저장소)

주의:
- 현재 UI `telegram-settings.html`의 Command Test/Approval Control은 `session_key` 없이 `/telegram/command`를 호출하므로 실패할 수 있음
- API 테스트는 `{"text":"...","session_key":"tg:chat:<id>"}` 형태로 호출

## 3) 승인(Approval) 계약
App Server 승인 요청 메서드:
- `item/commandExecution/requestApproval`
- `item/fileChange/requestApproval`
- `item/permissions/requestApproval`
- `execCommandApproval`
- `applyPatchApproval`

의사결정 정규화 (`NormalizeApprovalDecision`):
- `1` -> `accept`
- `2` -> `accept_for_session` (내부 App Server 전송값은 `acceptForSession`)
- `3` -> `decline`
- `4` -> `cancel`

Telegram callback 데이터:
- `approval:1|2|3|4:<id>`
- `apr:ok:<id>`, `apr:no:<id>`
- 구형 호환: `approve:<id>`, `deny:<id>`

## 4) 워크스페이스 정책 주입
- `workspace_dir`(settings) 또는 `MANTISX_WORKSPACE_DIR`가 있으면 프롬프트 앞에 정책 문자열 자동 주입
- 주입 문자열 형식:
  - `Workspace policy: Use only this workspace as project root: <dir>. Do not inspect unrelated repos.`
- 구현: `internal/mcp/workspace.go`

## 5) 백엔드 모드/Failover
- `MANTISX_BACKEND_MODE`:
  - `""|appserver|app-server|app_server` -> App Server
  - 그 외 -> RPC
- `MANTISX_FAILOVER_ENABLED=true`면 failover 래퍼 사용
- `MANTISX_FAILOVER_SECONDARY_MODE` 기본 `rpc`
- `MANTISX_FAILOVER_COOLDOWN_SEC` 기본 `300`

## 6) Telegram 계층 포인트
- 명령 라우팅: `internal/telegram/service.go`
- long polling/콜백/진행상태 업데이트: `internal/telegram/poller.go`
- 진행 표시 상수: `internal/telegram/progress.go`
  - `MinEditInterval=1.2s`
  - `HeartbeatInterval=6s`
  - `StallWarningAfter=20s`
- 승인 대기 시 인라인 버튼 자동 노출
- 응답은 chunk 분할 전송
  - 일반 chunk: `MANTISX_TG_CHUNK_CHARS` (기본 `1200`)
  - 최종 chunk: `MANTISX_TG_FINAL_CHUNK_CHARS` (기본 `3500`)

## 7) 설정/UI 포인트
- 저장소: `internal/settings/store.go`
- 설정 파일 기본: `state/telegram_settings.json`
- UI: `internal/web/static/telegram-settings.html`
- 저장 시 poller 재기동: `cmd/mantisx_server/main.go`의 `startPolling`
- 설정 API 인증:
  - `MANTISX_SETTINGS_TOKEN` 미설정 시 무인증
  - 설정 시 `X-Admin-Token` 또는 `Authorization: Bearer <token>` 필요

## 8) 운영 시 주의 포인트
- App Server `initialize` 응답은 method 없는 JSON-RPC response로 처리
- stdout JSON 파싱과 stderr 로그를 분리 처리
- RPC 경로는 daemon 출력 포맷별(JSON line/SSE/raw) 파싱 보정 로직 존재
- Telegram 송신 전 ANSI/제어문자 sanitize
- Windows `rg` 글롭 오용(`os error 123`)은 사용자 안내 문구로 변환
- RPC 세션 정리 루프는 `RPCController`일 때만 동작 (`MANTISX_SESSION_TTL_MIN`, 기본 120분)

## 9) 수정 시 우선 확인 파일
- 승인 UX/콜백 변경:
  - `internal/telegram/poller.go`
  - `internal/telegram/service.go`
  - `internal/mcp/app_server_controller.go`
  - `internal/mcp/rpc_controller.go`
- 세션/timeout 변경:
  - `cmd/mantisx_server/main.go`
  - `internal/mcp/session_context.go`
  - `internal/mcp/session_store.go`
- 설정 스키마/UI 변경:
  - `internal/settings/store.go`
  - `internal/web/static/telegram-settings.html`
  - `README.md`

## 10) 검증 명령
```powershell
cd C:\mantisx
go test ./internal/mcp ./internal/telegram ./cmd/mantisx_server
go test ./...
go build -o .\cmd\bin\mantisx_server.exe .\cmd\mantisx_server
Invoke-WebRequest http://127.0.0.1:18080/healthz
```

운영 메모:
- "컴파일 후 재실행" 요청에는 `cmd /c .\rebuild_restart_mantisx_stack.bat`를 우선 사용
- 이 스크립트는 빌드 성공 시에만 서버 종료, 2초 대기 후 재시작
