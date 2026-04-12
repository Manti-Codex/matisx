# MantisX Binary Distribution

MantisX는 Telegram으로 Codex를 원격 제어하는 실행파일 배포 패키지입니다.  
이 저장소는 소스코드 없이 운영에 필요한 최소 파일만 포함합니다.

## 포함 파일
- `mantisx_server.exe`
- `start_mantisx.bat`
- `tools/`
  - `tools/tools/manifest.json`
  - `tools/mcp/registry.json`
  - `tools/skills/registry.json`

## 1) 빠른 시작 (Windows)
1. Codex CLI 설치
```powershell
npm i -g @openai/codex
```
2. Codex 로그인
```powershell
codex login
```
3. 서버 시작
```powershell
.\start_mantisx.bat
```
4. 설정 UI 접속
- `http://127.0.0.1:18080/ui/telegram-settings.html`

## 2) Telegram 설정
설정 UI에서 아래를 입력 후 저장합니다.
- `Token`: BotFather에서 받은 텔레그램 봇 토큰
- `Allowed Users`: 허용할 Telegram 사용자/채팅 ID (쉼표 구분)
- `Workspace Dir`: Codex가 작업할 루트 경로

저장 후 Telegram에서 봇에게 `/start`를 보내면 연결 상태를 확인할 수 있습니다.

## 3) 주요 명령 (Telegram)
- `/help`: 사용 가능한 명령 안내
- `/clear`: 현재 MCP 세션 초기화
- `/stop_mcp`: 백엔드 중지
- `/approval on`, `/approval off`: 승인 모드 전환
- `1,2,3,4`: 승인 빠른 입력
  - `1=허용`, `2=세션 허용`, `3=거절`, `4=취소`
- `/memory`: 현재 메모리 상태 조회
- `/remember <text>`: 장기기억 저장
- `/memory_clear`: 현재 세션 메모리 삭제

## 4) 메모리 동작 (중요)
MantisX는 세션 단위로 3층 메모리를 사용합니다.

- 장기기억(Long)
  - 사용자 규칙/선호/고정 정보
  - `/remember`로 명시 저장
- 중기기억(Mid)
  - 최근 턴 요약 누적
  - 대화가 길어져도 맥락 유지용
- 임시기억(Temp)
  - 직전 대화 버퍼
  - 빠른 문맥 이어받기용

저장 위치:
- `state/memory_layers.json`

특징:
- 서버 재시작 후에도 파일에서 자동 복원
- 세션별로 분리되어 관리

## 5) 런타임 파일
서버 실행 후 `state/`가 자동 생성됩니다.
- `state/telegram_settings.json`: Telegram 설정 저장
- `state/memory_layers.json`: 메모리 저장

## 6) 장애 대응
### A. 설정 페이지 접속 불가
- 서버가 떠 있는지 확인: `http://127.0.0.1:18080/healthz`
- 방화벽/포트 점유 확인

### B. Codex 관련 오류
- `codex` 미설치/경로 문제:
  - `npm i -g @openai/codex`
  - 필요 시 환경변수 `MANTISX_CODEX_EXE` 지정
- 로그인 미완료:
  - `codex login`

### C. Telegram 응답 없음
- Token/Allowed Users 값 재확인
- Allowed Users에 실제 본인 ID가 있는지 확인
- 저장 후 봇에 `/start` 재전송

## 7) 운영 팁
- 배포 폴더는 `mantisx_server.exe`, `start_mantisx.bat`, `tools/`만 유지 권장
- `state/`는 운영 데이터이므로 백업 대상에 포함 권장
- 승인 모드는 기본적으로 켜고 운영하는 것을 권장

## 8) Git Push 규칙 (중요)
이 저장소는 배포 전용으로 아래 파일만 push합니다.
- `mantisx_server.exe`
- `README.md`
- `tools/` (하위 파일 포함)

push 금지:
- 소스코드 디렉터리 (`cmd/`, `internal/`, `docs/` 등)
- 실행 중 생성되는 데이터 (`state/`)
- 임시 작업 폴더 (`tmp_*`)
