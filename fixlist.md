# Fix List

## F001 - Approval 후 결과 연속성 (AppServer)
- 위치: `internal/mcp/app_server_controller.go`
- 상태: 부분 완화 완료 (짧은 wait 후 결과 회수)
- 남은 문제: 승인 후 완료 이벤트가 대기 시간 이후 도착하면 같은 요청에서 바로 전달되지 않을 수 있음.
- 권장 조치: `turnId` 기준 재개 전용 상태머신/이벤트 푸시 채널 추가.

## F002 - Failover 승인 상태 일관성
- 위치: `internal/mcp/failover_controller.go`
- 상태: 부분 완화 완료 (primary+secondary approval 목록 병합, resolve 양쪽 시도)
- 남은 문제: 백엔드 간 트랜잭션/영속 공유 없음. 동일/유사 ID 충돌 가능성 존재.
- 권장 조치: 공용 ApprovalStore(영속) 도입 + backend source 태깅.

## F003 - 세션 요청/승인 수명주기 분리 부족
- 위치: `cmd/mantisx_server/main.go`
- 상태: 완화 완료 (세션당 단일 active request -> 다중 request 추적)
- 남은 문제: 승인 대기 turn과 일반 turn의 큐/우선순위 상태머신은 아직 없음.
- 권장 조치: 세션별 작업 큐와 approval-pending 상태를 명시적으로 분리.

## F004 - RPC Legacy approval fallback
- 위치: `internal/mcp/rpc_controller.go`
- 상태: 유지(하위호환)
- 남은 문제: `Daemon.ResolveApproval` 미지원 시 자연어 follow-up으로 대체되어 paused-state와 어긋날 수 있음.
- 권장 조치: daemon 측 `ResolveApproval` 강제 지원 후 legacy 경로 제거.

## F005 - 임시 바이너리 백업 파일 삭제 실패
- 위치: `cmd/bin/mantisx_server.exe~`
- 상태: 미해결
- 원인: 파일 잠금/권한(`Access is denied`)
- 권장 조치: 점유 프로세스 종료 후 관리자 권한에서 삭제.

## F006 - 실행 중 daemon 로그 파일 정리
- 위치: `state/daemon.err.log`, `state/daemon.out.log`
- 상태: 부분 정리
- 남은 문제: 실행 중 프로세스가 파일을 점유 중이라 즉시 삭제 불가.
- 권장 조치: stack 중지 후 로그 롤링 스크립트로 정리.
