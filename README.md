# MantisX Binary Distribution

이 저장소는 소스코드 없이 실행파일 배포용입니다.

## 포함 파일
- `mantisx_server.exe`
- `tools/` (확장 설정/기술문서)
  - `tools/tools/manifest.json`
  - `tools/mcp/registry.json`
  - `tools/skills/registry.json`

## 실행 방법 (Windows)
1. `codex` 실행파일이 PATH에 있어야 합니다.
2. 아래처럼 서버 실행:

```powershell
$env:MANTISX_BACKEND_MODE='appserver'
$env:MANTISX_CODEX_EXE='codex'
.\mantisx_server.exe
```

3. 브라우저에서 설정 UI 접속:
- `http://127.0.0.1:18080/ui/telegram-settings.html`

## 확장 포인트
- `tools/tools`: 서버 툴 점검 기준(manifest)
- `tools/mcp`: 사용자 MCP 레지스트리
- `tools/skills`: 사용자 skill 레지스트리

## 참고
- `state/telegram_settings.json`, `state/memory_layers.json`는 실행 중 자동 생성됩니다.
- 이 저장소는 의도적으로 소스코드를 포함하지 않습니다.
