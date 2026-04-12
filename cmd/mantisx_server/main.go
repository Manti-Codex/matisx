package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"mantisx/internal/mcp"
	"mantisx/internal/memory"
	"mantisx/internal/settings"
	"mantisx/internal/telegram"
	"mantisx/internal/web"
)

type cmdReq struct {
	Text       string `json:"text"`
	SessionKey string `json:"session_key"`
	RequestID  string `json:"request_id"`
}

type cmdRes struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

func main() {
	addr := ":18080"
	if v := os.Getenv("MANTISX_HTTP_ADDR"); v != "" {
		addr = v
	}

	timeoutSec := 240
	if v := os.Getenv("MANTISX_COMMAND_TIMEOUT_SEC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			timeoutSec = n
		}
	}

	workDir, _ := os.Getwd()
	settingsPath := os.Getenv("MANTISX_SETTINGS_FILE")
	if strings.TrimSpace(settingsPath) == "" {
		settingsPath = filepath.Join(workDir, "state", "telegram_settings.json")
	}
	settingsStore := settings.NewStore(settingsPath)
	settingsToken := strings.TrimSpace(os.Getenv("MANTISX_SETTINGS_TOKEN"))
	memoryPath := strings.TrimSpace(os.Getenv("MANTISX_MEMORY_FILE"))
	if memoryPath == "" {
		memoryPath = filepath.Join(workDir, "state", "memory_layers.json")
	}

	memStore, err := memory.NewLayerStore(memoryPath)
	if err != nil {
		log.Printf("[mantisx] memory disabled (load failed): %v", err)
	}

	controller := mcp.NewControllerFromEnv()
	backendLabel, llmLabel, backendDetail, codexExe := runtimeLabels(controller)
	tg := telegram.Service{MCP: controller, Memory: memStore}
	sessionTTLMin := 120
	if v := os.Getenv("MANTISX_SESSION_TTL_MIN"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			sessionTTLMin = n
		}
	}
	sessionTTL := time.Duration(sessionTTLMin) * time.Minute
	cleanupStop := make(chan struct{})
	if rpcCtrl, ok := controller.(*mcp.RPCController); ok {
		go func() {
			tk := time.NewTicker(10 * time.Minute)
			defer tk.Stop()
			for {
				select {
				case <-cleanupStop:
					return
				case <-tk.C:
					rpcCtrl.CleanupSessions(sessionTTL)
				}
			}
		}()
	}

	var pollMu sync.Mutex
	var pollCancel context.CancelFunc
	var reqMu sync.Mutex
	// NOTE(fixlist#F003): request tracking is per-request now to avoid stale overwrite,
	// but there is still no dedicated queue/state machine for "approval paused turn" lifecycle.
	activeReqBySession := map[string]map[string]struct{}{}
	startPolling := func(cfg settings.TelegramSettings) {
		pollMu.Lock()
		defer pollMu.Unlock()
		if pollCancel != nil {
			pollCancel()
			pollCancel = nil
		}
		if strings.TrimSpace(cfg.CodexDaemonAddr) != "" {
			if rpcCtrl, ok := controller.(*mcp.RPCController); ok {
				rpcCtrl.SetAddr(strings.TrimSpace(cfg.CodexDaemonAddr))
			}
		}
		if ws, ok := controller.(mcp.WorkspaceSetter); ok {
			ws.SetWorkspaceDir(cfg.WorkspaceDir)
		}
		token := strings.TrimSpace(cfg.Token)
		if token == "" {
			log.Printf("[tg] polling not started: empty token")
			return
		}
		allowed := telegram.ParseAllowedUsers(cfg.AllowedUsers)
		notifyBoot := func() {
			if len(allowed) == 0 {
				return
			}
			msg := buildBootNotifyMessage(memStore, memoryPath, backendLabel, llmLabel)
			for _, chatID := range allowed {
				if chatID <= 0 {
					continue
				}
				if err := telegram.SendTextMessage(token, chatID, msg); err != nil {
					log.Printf("[tg] boot notify failed (chat_id=%d): %v", chatID, err)
					continue
				}
				log.Printf("[tg] boot notify sent (chat_id=%d)", chatID)
			}
		}
		notifyBoot()
		ctx, cancel := context.WithCancel(context.Background())
		pollCancel = cancel
		go telegram.RunPolling(ctx, telegram.PollingConfig{
			Token:         token,
			AllowedUsers:  allowed,
			Handler:       tg,
			HandleTimeout: time.Duration(timeoutSec) * time.Second,
		})
	}

	savedCfg := settings.TelegramSettings{}
	if saved, err := settingsStore.Load(); err == nil {
		savedCfg = saved
		startPolling(saved)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/telegram/command", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req cmdReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(cmdRes{OK: false, Message: "invalid json"})
			return
		}
		sessionKey := strings.TrimSpace(req.SessionKey)
		if sessionKey == "" {
			// UI backward compatibility: old command test payload might omit session_key.
			sessionKey = "ui:default"
		}
		requestID := strings.TrimSpace(req.RequestID)
		if requestID == "" {
			requestID = strconv.FormatInt(time.Now().UnixNano(), 10)
		}

		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(timeoutSec)*time.Second)
		defer cancel()
		ctx = mcp.WithSessionKey(ctx, sessionKey)

		reqMu.Lock()
		if activeReqBySession[sessionKey] == nil {
			activeReqBySession[sessionKey] = map[string]struct{}{}
		}
		activeReqBySession[sessionKey][requestID] = struct{}{}
		reqMu.Unlock()

		type result struct {
			msg string
			err error
		}
		resultCh := make(chan result, 1)
		go func(sessionKey, requestID string) {
			msg, err := tg.HandleText(ctx, req.Text)
			reqMu.Lock()
			_, active := activeReqBySession[sessionKey][requestID]
			reqMu.Unlock()
			if !active {
				// Timed-out/closed request: drop late result.
				return
			}
			resultCh <- result{msg: msg, err: err}
		}(sessionKey, requestID)
		select {
		case out := <-resultCh:
			reqMu.Lock()
			if m, ok := activeReqBySession[sessionKey]; ok {
				delete(m, requestID)
				if len(m) == 0 {
					delete(activeReqBySession, sessionKey)
				}
			}
			reqMu.Unlock()
			_ = json.NewEncoder(w).Encode(cmdRes{OK: out.err == nil, Message: out.msg})
		case <-ctx.Done():
			reqMu.Lock()
			if m, ok := activeReqBySession[sessionKey]; ok {
				delete(m, requestID)
				if len(m) == 0 {
					delete(activeReqBySession, sessionKey)
				}
			}
			reqMu.Unlock()
			_ = json.NewEncoder(w).Encode(cmdRes{
				OK:      false,
				Message: "요청 시간 초과: 백엔드 작업 결과는 무시됩니다. request_id=" + requestID,
			})
		}
	})
	mux.HandleFunc("/api/settings/telegram", func(w http.ResponseWriter, r *http.Request) {
		if !authorizedSettingsRequest(r, settingsToken) {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}
		switch r.Method {
		case http.MethodGet:
			cfg, err := settingsStore.Load()
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			_ = json.NewEncoder(w).Encode(cfg)
		case http.MethodPut:
			var req settings.TelegramSettings
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid json"})
				return
			}
			cfg, err := settingsStore.Save(req)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			startPolling(cfg)
			_ = json.NewEncoder(w).Encode(cfg)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	uiFS, err := fs.Sub(web.StaticFS, "static")
	if err != nil {
		log.Fatalf("static fs sub failed: %v", err)
	}
	mux.Handle("/ui/", http.StripPrefix("/ui/", http.FileServer(http.FS(uiFS))))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/ui/telegram-settings.html", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	})

	printStartupBanner(addr, backendLabel, llmLabel, backendDetail, codexExe, savedCfg)

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	go func() {
		log.Printf("[mantisx] listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Printf("[mantisx] shutdown signal received")
	signal.Stop(sigCh)

	pollMu.Lock()
	if pollCancel != nil {
		pollCancel()
		pollCancel = nil
	}
	pollMu.Unlock()
	close(cleanupStop)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("[mantisx] http shutdown error: %v", err)
	}
}

func authorizedSettingsRequest(r *http.Request, token string) bool {
	if strings.TrimSpace(token) == "" {
		return true
	}
	if strings.TrimSpace(r.Header.Get("X-Admin-Token")) == token {
		return true
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		if strings.TrimSpace(auth[len("Bearer "):]) == token {
			return true
		}
	}
	return false
}

func buildBootNotifyMessage(memStore *memory.LayerStore, memoryPath string, backendLabel string, llmLabel string) string {
	memInfo := "메모리: 비활성"
	if memStore != nil {
		memInfo = fmt.Sprintf("메모리: 로드됨 (sessions=%d)", memStore.SessionCount())
	}
	return strings.TrimSpace(strings.Join([]string{
		"🦗 MantisX 재시작 완료",
		"- 상태: polling 시작",
		"- backend: " + backendLabel,
		"- LLM: " + llmLabel,
		"- " + memInfo,
		"- memory file: " + memoryPath,
	}, "\n"))
}

func runtimeLabels(controller mcp.Controller) (backendLabel string, llmLabel string, backendDetail string, codexExe string) {
	backendLabel = fmt.Sprintf("%T", controller)
	llmLabel = "codex (unknown mode)"
	switch c := controller.(type) {
	case *mcp.RPCController:
		backendLabel = "rpc"
		llmLabel = "codex via daemon rpc"
		backendDetail = "daemon addr=" + c.Addr
	case *mcp.AppServerController:
		backendLabel = "app-server"
		llmLabel = "codex app-server"
		backendDetail = "codex_exe=" + c.CodexExe
		codexExe = strings.TrimSpace(c.CodexExe)
	}
	return backendLabel, llmLabel, backendDetail, codexExe
}

func printStartupBanner(
	addr string,
	backendLabel string,
	llmLabel string,
	backendDetail string,
	codexExe string,
	cfg settings.TelegramSettings,
) {
	useColor := envBool("MANTISX_BANNER_COLOR", false)
	color := func(code string, s string) string {
		if !useColor {
			return s
		}
		return code + s + "\033[0m"
	}
	httpAddr := strings.TrimSpace(addr)
	if httpAddr == "" {
		httpAddr = ":18080"
	}
	baseURL := "http://127.0.0.1" + httpAddr
	settingsURL := baseURL + "/ui/telegram-settings.html"
	tgID := firstAllowedUser(cfg.AllowedUsers)
	if tgID == "" {
		tgID = "(없음 - Settings UI에서 allowed_users 설정)"
	}
	tokenLine := "missing (Settings UI에서 Telegram token 설정 필요)"
	if strings.TrimSpace(cfg.Token) != "" {
		tokenLine = maskToken(cfg.Token)
	}
	codexBinLine := "확인 불가"
	codexBinFix := ""
	if strings.TrimSpace(codexExe) != "" {
		if resolved, err := resolveExecutable(codexExe); err == nil {
			codexBinLine = "OK (" + resolved + ")"
		} else {
			codexBinLine = "NOT FOUND (" + codexExe + ")"
			codexBinFix = "codex 설치/경로 확인: `npm i -g @openai/codex` 또는 MANTISX_CODEX_EXE 수정"
		}
	}
	codexLoginLine, codexLoginFix := detectCodexLoginStatus()
	detailLine := ""
	if strings.TrimSpace(backendDetail) != "" && strings.TrimSpace(codexExe) == "" {
		detailLine = "  - Detail    : " + backendDetail + "\n"
	}
	banner := "\n" +
		"================================================================\n" +
		color("\033[1;32m", " __  ___            __  _      _  __") + "\n" +
		color("\033[1;32m", "/  |/  /___ _____  / /_(_)____| |/ /") + "\n" +
		color("\033[1;32m", "/ /|_/ / __ `/ __ \\/ __/ / ___/   / ") + "\n" +
		color("\033[1;32m", "/ /  / / /_/ / / / / /_/ (__  )   |  ") + "\n" +
		color("\033[1;32m", "/_/  /_/\\__,_/_/ /_/\\__/_/____/_/|_|  ") + "\n" +
		"\n" +
		"                        " + color("\033[1;36m", "MantisX") + "\n" +
		"                 " + color("\033[36m", "Remote Codex Controller") + "\n" +
		"================================================================\n" +
		fmt.Sprintf("  [RUNNING] Port      : %s\n", httpAddr) +
		fmt.Sprintf("  [OK]      Backend   : %s\n", backendLabel) +
		fmt.Sprintf("  [OK]      LLM       : %s\n", llmLabel) +
		fmt.Sprintf("  [SETUP]   Open      : %s\n", settingsURL) +
		fmt.Sprintf("  [TG]      ID        : %s\n", tgID) +
		fmt.Sprintf("  [TG]      Token     : %s\n", tokenLine) +
		fmt.Sprintf("  [CODEX]   Binary    : %s\n", codexBinLine) +
		fmt.Sprintf("  [CODEX]   Login     : %s\n", codexLoginLine) +
		detailLine +
		"----------------------------------------------------------------\n" +
		"  [ACTION]  Telegram 미설정이면 Settings UI에서 Token/allowed_users 입력 후 Save\n" +
		"  [ACTION]  Codex 로그인 필요 시 터미널에서 `codex login` 실행\n" +
		"================================================================\n"
	if codexBinFix != "" {
		banner += "  [FIX]     " + codexBinFix + "\n"
	}
	if codexLoginFix != "" {
		banner += "  [FIX]     " + codexLoginFix + "\n"
	}
	log.Print(banner)
}

func firstAllowedUser(raw string) string {
	users := telegram.ParseAllowedUsers(raw)
	if len(users) == 0 {
		return ""
	}
	return strconv.FormatInt(users[0], 10)
}

func maskToken(token string) string {
	t := strings.TrimSpace(token)
	if t == "" {
		return ""
	}
	if len(t) <= 10 {
		return t
	}
	return t[:6] + "..." + t[len(t)-4:]
}

func envBool(key string, def bool) bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if raw == "" {
		return def
	}
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

func resolveExecutable(exe string) (string, error) {
	if strings.TrimSpace(exe) == "" {
		return "", errors.New("empty executable")
	}
	if strings.ContainsAny(exe, `/\`) {
		if st, err := os.Stat(exe); err == nil && !st.IsDir() {
			return exe, nil
		} else if err != nil {
			return "", err
		}
	}
	return exec.LookPath(exe)
}

func detectCodexLoginStatus() (string, string) {
	if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) != "" {
		return "OK (OPENAI_API_KEY)", ""
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		authCandidates := []string{
			filepath.Join(home, ".codex", "auth.json"),
			filepath.Join(home, ".codex", "credentials.json"),
		}
		for _, p := range authCandidates {
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return "OK (cached credentials)", ""
			}
		}
	}
	return "NOT LOGGED IN", "터미널에서 `codex login` 또는 OPENAI_API_KEY 설정"
}
