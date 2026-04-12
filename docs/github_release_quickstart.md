# GitHub 배포 준비 (실행파일 중심)

## 1) GitHub 가입
1. 브라우저에서 `https://github.com/signup` 접속
2. 계정 생성 후 이메일 인증 완료
3. 필요하면 `https://github.com/settings/tokens`에서 PAT 발급 (`repo` 권한)

## 2) 로컬 초기 커밋
```powershell
cd C:\mantisx
git add .
git commit -m "chore: initial mantisx project setup"
```

## 3) GitHub 저장소 생성 + 연결
CLI 방식(`gh`) 예시:
```powershell
gh auth login
gh repo create <OWNER>/mantisx --private --source . --remote origin --push
```

수동 방식:
1. GitHub 웹에서 빈 저장소 생성
2. 아래 실행
```powershell
git remote add origin https://github.com/<OWNER>/mantisx.git
git push -u origin master
```

## 4) 실행파일 릴리즈 배포
이 프로젝트는 태그 `vX.Y.Z`를 푸시하면 GitHub Actions가 Windows 실행파일을 만들어 Release에 업로드합니다.

```powershell
git tag v0.1.0
git push origin v0.1.0
```

업로드 파일:
- `mantisx_server_windows_amd64.exe`
- `SHA256SUMS.txt`

