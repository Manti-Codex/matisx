# 운영 핸드오프

## 기준 문서
- 메인 가이드: `agent.md`
- 운영 절차: `docs/daemon_server_ops.md`

## 현재 코드 기준 핵심
- 서버 엔트리: `cmd/mantisx_server/main.go`
- 백엔드 기본 모드: `appserver` (`internal/mcp/factory.go`)
- 백엔드 구현:
  - AppServer: `internal/mcp/app_server_controller.go`
  - RPC: `internal/mcp/rpc_controller.go`
- Telegram 처리:
  - 명령 라우팅: `internal/telegram/service.go`
  - Long polling/콜백: `internal/telegram/poller.go`
- 설정 저장: `internal/settings/store.go`

## 변경 시 필수 동기화
- Telegram 명령 변경: `service.go` + `poller.go` + 관련 테스트
- 설정 스키마 변경: `store.go` + `internal/web/static/telegram-settings.html`
- 운영 기본값 변경: `main.go`, `factory.go`, 각 컨트롤러 + `README.md`/`docs`

## 운영 목표
- Telegram 명령을 통해 Codex 작업을 안정적으로 중계
- 승인(approval) 플로우를 Telegram에서 즉시 처리
- workspace 정책 주입으로 작업 범위 고정
