package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"mantisx/internal/mcp"
)

type PollingConfig struct {
	Token          string
	AllowedUsers   []int64
	Handler        Service
	HandleTimeout  time.Duration
	PollTimeoutSec int
}

type tgGetUpdatesReq struct {
	Offset  int64 `json:"offset,omitempty"`
	Timeout int   `json:"timeout,omitempty"`
}

type tgSendMsgReq struct {
	ChatID      int64  `json:"chat_id"`
	Text        string `json:"text"`
	ReplyMarkup any    `json:"reply_markup,omitempty"`
}

type tgEditMsgReq struct {
	ChatID      int64  `json:"chat_id"`
	MessageID   int64  `json:"message_id"`
	Text        string `json:"text"`
	ReplyMarkup any    `json:"reply_markup,omitempty"`
}

type inlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

type inlineKeyboardMarkup struct {
	InlineKeyboard [][]inlineKeyboardButton `json:"inline_keyboard"`
}

type tgResp[T any] struct {
	OK          bool   `json:"ok"`
	Result      T      `json:"result"`
	Description string `json:"description"`
	ErrorCode   int    `json:"error_code"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

type tgMessageResult struct {
	MessageID int64 `json:"message_id"`
}

type tgUpdate struct {
	UpdateID int64 `json:"update_id"`
	Message  struct {
		MessageID int64 `json:"message_id"`
		From      struct {
			ID int64 `json:"id"`
		} `json:"from"`
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		Text string `json:"text"`
	} `json:"message"`
	CallbackQuery *struct {
		ID   string `json:"id"`
		From struct {
			ID int64 `json:"id"`
		} `json:"from"`
		Message struct {
			Chat struct {
				ID int64 `json:"id"`
			} `json:"chat"`
			MessageID int64 `json:"message_id"`
		} `json:"message"`
		Data string `json:"data"`
	} `json:"callback_query,omitempty"`
}

type progressState struct {
	Phase           string
	AssistantBuffer string
	CommandBuffer   string
	LastRenderAt    time.Time
	LastHeartbeatAt time.Time
	StatusMessageID int64
}

type telegramAPIError struct {
	StatusCode int
	Code       int
	Desc       string
	RetryAfter int
}

var ansiEscapeRE = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
var ansiOrphanSGRRE = regexp.MustCompile(`\[(?:\d{1,3}(?:;\d{1,3})*)m`)

const (
	MinEditInterval   = 2 * time.Second
	HeartbeatInterval = 6 * time.Second
)

func (e *telegramAPIError) Error() string {
	if e == nil {
		return "telegram api error"
	}
	if e.Code != 0 {
		return fmt.Sprintf("telegram api error code=%d status=%d desc=%s", e.Code, e.StatusCode, e.Desc)
	}
	return fmt.Sprintf("telegram api http status=%d desc=%s", e.StatusCode, e.Desc)
}

func RunPolling(ctx context.Context, cfg PollingConfig) {
	token := strings.TrimSpace(cfg.Token)
	if token == "" {
		log.Printf("[tg] polling disabled: empty token")
		return
	}
	if cfg.HandleTimeout <= 0 {
		cfg.HandleTimeout = 120 * time.Second
	}
	if cfg.PollTimeoutSec <= 0 {
		cfg.PollTimeoutSec = envInt("MANTISX_TG_POLL_TIMEOUT_SEC", 30)
	}
	if cfg.PollTimeoutSec < 10 {
		cfg.PollTimeoutSec = 10
	}
	if cfg.PollTimeoutSec > 50 {
		cfg.PollTimeoutSec = 50
	}
	chunkChars := envInt("MANTISX_TG_CHUNK_CHARS", 1200)
	if chunkChars < 300 {
		chunkChars = 300
	}
	if chunkChars > 3500 {
		chunkChars = 3500
	}
	finalChunkChars := envInt("MANTISX_TG_FINAL_CHUNK_CHARS", 3500)
	if finalChunkChars < 500 {
		finalChunkChars = 500
	}
	if finalChunkChars > 3500 {
		finalChunkChars = 3500
	}
	hardTimeoutSec := envInt("MANTISX_TG_HARD_TIMEOUT_SEC", 0)

	api := "https://api.telegram.org/bot" + token
	pollClient := &http.Client{Timeout: time.Duration(cfg.PollTimeoutSec+10) * time.Second}
	writeTimeoutSec := envInt("MANTISX_TG_WRITE_TIMEOUT_SEC", 8)
	if writeTimeoutSec < 3 {
		writeTimeoutSec = 3
	}
	if writeTimeoutSec > 20 {
		writeTimeoutSec = 20
	}
	writeClient := &http.Client{Timeout: time.Duration(writeTimeoutSec) * time.Second}

	allowedSet := map[int64]struct{}{}
	for _, id := range cfg.AllowedUsers {
		if id > 0 {
			allowedSet[id] = struct{}{}
		}
	}

	offset := latestUpdateOffset(pollClient, api)
	log.Printf("[tg] polling started (allowed_users=%d, offset=%d, timeout=%ds)", len(allowedSet), offset, cfg.PollTimeoutSec)

	errStreak := 0
	for {
		select {
		case <-ctx.Done():
			log.Printf("[tg] polling stopped")
			return
		default:
		}

		updates, err := getUpdates(pollClient, api, offset, cfg.PollTimeoutSec)
		if err != nil {
			errStreak++
			sleep := backoffDuration(errStreak, err)
			log.Printf("[tg] getUpdates error (streak=%d, sleep=%s): %v", errStreak, sleep, err)
			time.Sleep(sleep)
			continue
		}
		errStreak = 0
		if len(updates) == 0 {
			continue
		}

		sort.Slice(updates, func(i, j int) bool { return updates[i].UpdateID < updates[j].UpdateID })
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			if u.CallbackQuery != nil {
				cb := u.CallbackQuery
				if len(allowedSet) > 0 {
					if _, ok := allowedSet[cb.From.ID]; !ok {
						_ = answerCallback(writeClient, api, cb.ID, "not allowed")
						continue
					}
				}
				msg, err := handleCallback(cfg, cb.Message.Chat.ID, cb.Data)
				if err != nil {
					msg = "Request failed: " + err.Error()
				}
				if strings.TrimSpace(msg) != "" {
					_ = editMessage(writeClient, api, cb.Message.Chat.ID, cb.Message.MessageID, msg, nil)
					maybeStartApprovalFollowup(writeClient, api, cfg, cb.Message.Chat.ID, msg)
				}
				_ = answerCallback(writeClient, api, cb.ID, "ok")
				continue
			}

			chatID := u.Message.Chat.ID
			fromID := u.Message.From.ID
			text := strings.TrimSpace(u.Message.Text)
			if chatID == 0 || text == "" {
				continue
			}
			if len(allowedSet) > 0 {
				if _, ok := allowedSet[fromID]; !ok {
					continue
				}
			}

			pendingID, _ := sendMessage(writeClient, api, chatID, "처리 중...\n- 현재: 분석 중", nil)
			started := time.Now()
			ps := progressState{
				Phase:           "thinking",
				LastRenderAt:    started,
				LastHeartbeatAt: started,
				StatusMessageID: pendingID,
			}
			resultCh := make(chan struct {
				resp string
				err  error
			}, 1)

			hctx, hcancel := context.WithTimeout(ctx, cfg.HandleTimeout)
			go func(input string) {
				defer hcancel()
				resp, err := cfg.Handler.HandleText(hctx, input)
				resultCh <- struct {
					resp string
					err  error
				}{resp: resp, err: err}
			}(text)

			ticker := time.NewTicker(1 * time.Second)
			hardTimeout := cfg.HandleTimeout + 15*time.Second
			if hardTimeoutSec > 0 {
				hardTimeout = time.Duration(hardTimeoutSec) * time.Second
			}
			if hardTimeout < 30*time.Second {
				hardTimeout = 30 * time.Second
			}
			deadline := time.NewTimer(hardTimeout)
			var finalResp string
			var finalErr error
		waitLoop:
			for {
				select {
				case done := <-resultCh:
					finalResp = done.resp
					finalErr = done.err
					break waitLoop
				case <-ticker.C:
					if ps.StatusMessageID > 0 {
						now := time.Now()
						elapsed := int(now.Sub(started).Seconds())
						progressText, progressKB, phase := buildProgressMessageAndKeyboard(
							elapsed,
							int(cfg.HandleTimeout.Seconds()),
							int(hardTimeout.Seconds()),
							cfg.Handler,
							chatID,
							text,
						)
						if phase != "" && phase != "ignore" {
							ps.Phase = phase
						}
						if phase == "ignore" && strings.TrimSpace(ps.Phase) != "" {
							progressText = renderProgressLine(
								statusByElapsed(elapsed, int(cfg.HandleTimeout.Seconds()), int(hardTimeout.Seconds())),
								ps.Phase,
								"",
								elapsed,
							)
						}
						// minimum edit interval and heartbeat are fixed to avoid chat flood while showing liveness.
						if now.Sub(ps.LastRenderAt) >= MinEditInterval || now.Sub(ps.LastHeartbeatAt) >= HeartbeatInterval {
							_ = editMessage(writeClient, api, chatID, ps.StatusMessageID, progressText, progressKB)
							ps.LastRenderAt = now
							if now.Sub(ps.LastHeartbeatAt) >= HeartbeatInterval {
								ps.LastHeartbeatAt = now
							}
						}
					}
				case <-deadline.C:
					hcancel()
					finalErr = errorsNew("telegram handler timeout")
					finalResp = "Codex 응답 대기 시간이 초과되어 요청을 종료했습니다. 다시 시도해 주세요."
					log.Printf("[tg] handle timeout reached (chat_id=%d, input=%q, timeout=%s)", chatID, shortText(text, 120), hardTimeout)
					break waitLoop
				case <-ctx.Done():
					hcancel()
					finalErr = ctx.Err()
					finalResp = "서버가 종료되어 요청이 중단되었습니다."
					break waitLoop
				}
			}
			ticker.Stop()
			if !deadline.Stop() {
				select {
				case <-deadline.C:
				default:
				}
			}

			if finalErr != nil && strings.TrimSpace(finalResp) == "" {
				finalResp = "Request failed: " + finalErr.Error()
			}
			finalResp = strings.TrimSpace(finalResp)
			if finalResp == "" || strings.EqualFold(finalResp, "No response received. Please try again.") {
				if waitMsg, ok := approvalWaitMessage(cfg.Handler, chatID); ok {
					finalResp = waitMsg
				}
			}
			if finalResp == "" {
				finalResp = "No response received. Please try again."
			}

			parts := splitTelegramText(finalResp, finalChunkChars)
			if len(parts) == 0 {
				parts = []string{finalResp}
			}
			keyboard := buildInlineKeyboardForResponse(parts[0], cfg.Handler, chatID)
			if err := deliverFinalMessage(writeClient, api, chatID, pendingID, parts[0], keyboard); err != nil {
				log.Printf("[tg] final message delivery failed (chat_id=%d): %v", chatID, err)
			}
			maybeStartApprovalFollowup(writeClient, api, cfg, chatID, parts[0])
			for i := 1; i < len(parts); i++ {
				if strings.TrimSpace(parts[i]) == "" {
					continue
				}
				if _, err := sendMessage(writeClient, api, chatID, parts[i], nil); err != nil {
					log.Printf("[tg] chunk send failed (chat_id=%d, idx=%d/%d): %v", chatID, i+1, len(parts), err)
					break
				}
				time.Sleep(150 * time.Millisecond)
			}
		}
	}
}

func maybeStartApprovalFollowup(client *http.Client, api string, cfg PollingConfig, chatID int64, message string) {
	msg := strings.ToLower(strings.TrimSpace(message))
	if !(strings.Contains(msg, "approved:") || strings.Contains(msg, "(accept)")) {
		return
	}
	go func() {
		msgID, err := sendMessage(client, api, chatID, "처리 계속 중...\n- 현재: 분석 중", nil)
		if err != nil || msgID <= 0 {
			return
		}
		start := time.Now()
		tk := time.NewTicker(2 * time.Second)
		defer tk.Stop()
		for {
			elapsed := int(time.Since(start).Seconds())
			if elapsed > 40 {
				return
			}
			phase, task := readProgressPhaseAndTask(cfg.Handler, chatID)
			line := renderProgressLine(statusByElapsed(elapsed, 20, 40), phase, task, elapsed)
			_ = editMessage(client, api, chatID, msgID, line, nil)
			if phase == "final_answer" || phase == "approval_needed" {
				return
			}
			<-tk.C
		}
	}()
}

func ParseAllowedUsers(raw string) []int64 {
	parts := strings.Split(raw, ",")
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		v := strings.TrimSpace(p)
		if v == "" {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			continue
		}
		out = append(out, n)
	}
	return out
}

func latestUpdateOffset(client *http.Client, api string) int64 {
	updates, err := getUpdates(client, api, 0, 0)
	if err != nil || len(updates) == 0 {
		return 0
	}
	var maxID int64
	for _, u := range updates {
		if u.UpdateID > maxID {
			maxID = u.UpdateID
		}
	}
	return maxID + 1
}

func getUpdates(client *http.Client, api string, offset int64, timeoutSec int) ([]tgUpdate, error) {
	payload := tgGetUpdatesReq{Offset: offset, Timeout: timeoutSec}
	var out tgResp[[]tgUpdate]
	if err := postJSON(client, api+"/getUpdates", payload, &out); err != nil {
		return nil, err
	}
	if !out.OK {
		return nil, &telegramAPIError{Code: out.ErrorCode, Desc: out.Description, RetryAfter: out.Parameters.RetryAfter}
	}
	return out.Result, nil
}

func sendMessage(client *http.Client, api string, chatID int64, text string, replyMarkup any) (int64, error) {
	payload := tgSendMsgReq{ChatID: chatID, Text: sanitizeTelegramText(text), ReplyMarkup: replyMarkup}
	maxAttempts := 3
	for i := 1; i <= maxAttempts; i++ {
		var out tgResp[tgMessageResult]
		err := postJSON(client, api+"/sendMessage", payload, &out)
		if err == nil && out.OK {
			return out.Result.MessageID, nil
		}
		if err == nil && !out.OK {
			err = &telegramAPIError{Code: out.ErrorCode, Desc: out.Description, RetryAfter: out.Parameters.RetryAfter}
		}
		if payload.ReplyMarkup != nil && isReplyMarkupError(err) {
			payload.ReplyMarkup = nil
			continue
		}
		if i == maxAttempts {
			return 0, err
		}
		time.Sleep(backoffDuration(i, err))
	}
	return 0, nil
}

func editMessage(client *http.Client, api string, chatID, messageID int64, text string, replyMarkup any) error {
	if messageID <= 0 {
		return errorsNew("invalid message id")
	}
	payload := tgEditMsgReq{ChatID: chatID, MessageID: messageID, Text: sanitizeTelegramText(text), ReplyMarkup: replyMarkup}
	var out tgResp[tgMessageResult]
	err := postJSON(client, api+"/editMessageText", payload, &out)
	if err == nil && out.OK {
		return nil
	}
	if err == nil {
		return &telegramAPIError{Code: out.ErrorCode, Desc: out.Description, RetryAfter: out.Parameters.RetryAfter}
	}
	if payload.ReplyMarkup != nil && isReplyMarkupError(err) {
		payload.ReplyMarkup = nil
		var out2 tgResp[tgMessageResult]
		err2 := postJSON(client, api+"/editMessageText", payload, &out2)
		if err2 == nil && out2.OK {
			return nil
		}
		if err2 == nil {
			return &telegramAPIError{Code: out2.ErrorCode, Desc: out2.Description, RetryAfter: out2.Parameters.RetryAfter}
		}
		return err2
	}
	return err
}

func postJSON(client *http.Client, url string, reqBody any, out any) error {
	b, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if len(body) > 0 {
		_ = json.Unmarshal(body, out)
	}

	if resp.StatusCode >= 400 {
		apiErr := &telegramAPIError{StatusCode: resp.StatusCode, Desc: strings.TrimSpace(string(body))}
		var parsed tgResp[map[string]any]
		if json.Unmarshal(body, &parsed) == nil {
			apiErr.Code = parsed.ErrorCode
			apiErr.Desc = parsed.Description
			apiErr.RetryAfter = parsed.Parameters.RetryAfter
		}
		return apiErr
	}
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, out)
}

func backoffDuration(streak int, err error) time.Duration {
	if apiErr, ok := err.(*telegramAPIError); ok {
		if apiErr.RetryAfter > 0 {
			return time.Duration(apiErr.RetryAfter) * time.Second
		}
		if apiErr.Code == 429 || apiErr.StatusCode == 429 {
			return 3 * time.Second
		}
	}
	if streak < 1 {
		streak = 1
	}
	if streak > 5 {
		streak = 5
	}
	sec := int(math.Pow(2, float64(streak-1)))
	if sec > 8 {
		sec = 8
	}
	return time.Duration(sec) * time.Second
}

func splitTelegramText(s string, limit int) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return []string{""}
	}
	r := []rune(s)
	if len(r) <= limit {
		return []string{s}
	}
	var out []string
	for len(r) > 0 {
		if len(r) <= limit {
			out = append(out, string(r))
			break
		}
		cut := limit
		chunk := string(r[:cut])
		if idx := strings.LastIndex(chunk, "\n"); idx > limit/2 {
			chunk = chunk[:idx]
			cut = len([]rune(chunk))
		}
		out = append(out, strings.TrimSpace(chunk))
		r = r[cut:]
	}
	return out
}

func shortText(s string, limit int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= limit {
		return string(r)
	}
	return string(r[:limit]) + "..."
}

func envInt(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func errorsNew(msg string) error { return fmt.Errorf("%s", msg) }

func deliverFinalMessage(client *http.Client, api string, chatID, pendingID int64, text string, replyMarkup any) error {
	if pendingID > 0 {
		if err := editMessage(client, api, chatID, pendingID, text, replyMarkup); err == nil {
			return nil
		}
	}
	if _, err := sendMessage(client, api, chatID, text, replyMarkup); err == nil {
		return nil
	}
	time.Sleep(500 * time.Millisecond)
	_, err := sendMessage(client, api, chatID, text, replyMarkup)
	return err
}

func answerCallback(client *http.Client, api, callbackID, text string) error {
	payload := map[string]any{
		"callback_query_id": callbackID,
		"text":              text,
	}
	var out tgResp[map[string]any]
	return postJSON(client, api+"/answerCallbackQuery", payload, &out)
}

func buildProgressMessageAndKeyboard(elapsedSec, softSec, hardSec int, svc Service, chatID int64, inputHint string) (string, any, string) {
	if softSec <= 0 {
		softSec = 120
	}
	if hardSec <= 0 {
		hardSec = softSec + 15
	}
	status := statusByElapsed(elapsedSec, softSec, hardSec)
	phase, task := readProgressPhaseAndTask(svc, chatID)
	if strings.TrimSpace(task) == "" {
		task = strings.TrimSpace(inputHint)
	}
	progressLine := renderProgressLine(status, phase, task, elapsedSec)

	approvals, err := svc.MCP.ListApprovals(mcp.WithSessionKey(context.Background(), sessionKey(chatID)))
	if err != nil || len(approvals) == 0 {
		return progressLine, nil, phase
	}
	var rows [][]inlineKeyboardButton
	var lines []string
	lines = append(lines, "승인이 필요합니다.")
	maxRows := len(approvals)
	if maxRows > 3 {
		maxRows = 3
	}
	for i := 0; i < maxRows; i++ {
		ap := approvals[i]
		kindLabel := "일반"
		switch ap.Kind {
		case mcp.ApprovalKindCommand:
			kindLabel = "명령 실행"
		case mcp.ApprovalKindFileChange:
			kindLabel = "파일 변경"
		}
		lines = append(lines, fmt.Sprintf("- %s [%s]", ap.ID, kindLabel))
		rows = append(rows, []inlineKeyboardButton{
			{Text: "허용 " + ap.ID, CallbackData: approvalCallbackData(true, ap.ID)},
			{Text: "거절 " + ap.ID, CallbackData: approvalCallbackData(false, ap.ID)},
		})
	}
	if len(approvals) > maxRows {
		lines = append(lines, fmt.Sprintf("- 외 %d건: /approvals 로 전체 확인", len(approvals)-maxRows))
	}
	lines = append(lines, "- 버튼 또는 /approve <id>, /deny <id> 사용")
	return strings.Join(lines, "\n"), inlineKeyboardMarkup{InlineKeyboard: rows}, "approval_needed"
}

func statusByElapsed(elapsedSec, softSec, hardSec int) string {
	if elapsedSec >= softSec && elapsedSec < hardSec {
		return "TIMEOUT_EXTENDED"
	}
	if elapsedSec >= softSec-10 {
		return "TIMEOUT_WARNING"
	}
	if elapsedSec >= 60 {
		return "RECOVERING"
	}
	return "THINKING"
}

func inferSystemStatus(elapsedSec, softSec, hardSec int) string {
	return statusByElapsed(elapsedSec, softSec, hardSec)
}

func readProgressPhaseAndTask(svc Service, chatID int64) (string, string) {
	ctx := mcp.WithSessionKey(context.Background(), sessionKey(chatID))
	r, ok := svc.MCP.(mcp.ProgressReporter)
	if !ok {
		return "thinking", ""
	}
	st, err := r.GetProgress(ctx)
	if err != nil {
		return "thinking", ""
	}
	return strings.TrimSpace(st.Phase), strings.TrimSpace(st.Task)
}

func renderProgressLine(systemStatus, phase, task string, elapsedSec int) string {
	phaseLabel := "분석 중"
	taskHint := strings.TrimSpace(task)
	fallbackHint := rotatingFallbackHint(strings.ToLower(strings.TrimSpace(phase)), elapsedSec)
	switch strings.ToLower(strings.TrimSpace(phase)) {
	case "thinking":
		phaseLabel = classifyThinkingStatus(task)
		if words := rotatingInputWords(taskHint, elapsedSec); words != "" {
			phaseLabel = "분석 중 - " + words
		} else if fallbackHint != "" {
			phaseLabel = "분석 중 - " + fallbackHint
		}
	case "tool_running":
		phaseLabel = classifyToolStatus(task)
		if words := rotatingInputWords(taskHint, elapsedSec); words != "" {
			phaseLabel = phaseLabel + " - " + words
		} else if fallbackHint != "" {
			phaseLabel = phaseLabel + " - " + fallbackHint
		}
	case "approval_needed":
		phaseLabel = "승인이 필요합니다"
	case "final_answer":
		phaseLabel = "답변 정리 완료"
	case "ignore":
		return "처리 중...\n- 현재: 분석 중"
	}
	lines := []string{
		"처리 중...",
		"- 현재: " + phaseLabel,
	}
	if taskHint != "" && phase != "approval_needed" && phase != "final_answer" {
		lines = append(lines, "- 작업: "+shortText(taskHint, 48))
	}
	if systemStatus == "TIMEOUT_WARNING" {
		lines = append(lines, "- 상태: 지연 감지")
	}
	return strings.Join(lines, "\n")
}

func rotatingInputWords(task string, elapsedSec int) string {
	src := strings.TrimSpace(task)
	if src == "" {
		return ""
	}
	words := strings.Fields(src)
	if len(words) == 0 {
		return ""
	}
	for i := range words {
		words[i] = strings.Trim(words[i], ".,!?;:'\"()[]{}<>")
	}
	clean := make([]string, 0, len(words))
	for _, w := range words {
		if w != "" {
			clean = append(clean, w)
		}
	}
	if len(clean) == 0 {
		return ""
	}
	start := 0
	if elapsedSec > 0 {
		start = (elapsedSec / 2) % len(clean)
	}
	count := 3
	if len(clean) < count {
		count = len(clean)
	}
	out := make([]string, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, clean[(start+i)%len(clean)])
	}
	return strings.Join(out, " ")
}

func rotatingFallbackHint(phase string, elapsedSec int) string {
	thinking := []string{
		"gitDiffToRemote",
		"fuzzyFileSearch",
		"thread/read",
		"config/read",
		"model/list",
		"thread/list",
		"thread/loaded/list",
		"skills/list",
		"plugin/list",
		"app/list",
		"mcpServerStatus/list",
		"account/read",
		"getConversationSummary",
		"fuzzyFileSearch/sessionStart",
		"fuzzyFileSearch/sessionUpdate",
		"configRequirements/read",
		"externalAgentConfig/detect",
	}
	tool := []string{
		"command/exec",
		"fs/readFile",
		"thread/shellCommand",
		"gitDiffToRemote",
		"command/exec/terminate",
		"command/exec/resize",
		"fs/readDirectory",
		"fs/getMetadata",
		"fs/createDirectory",
		"fs/copy",
		"thread/rollback",
		"thread/backgroundTerminals/clean",
		"review/start",
		"thread/realtime/start",
		"thread/realtime/appendText",
		"thread/realtime/appendAudio",
		"thread/realtime/stop",
	}
	src := thinking
	if phase == "tool_running" {
		src = tool
	}
	if len(src) == 0 {
		return ""
	}
	idx := 0
	if elapsedSec > 0 {
		idx = (elapsedSec / 2) % len(src)
	}
	return src[idx]
}

func classifyThinkingStatus(task string) string {
	t := strings.ToLower(strings.TrimSpace(task))
	switch {
	case strings.Contains(t, "file"), strings.Contains(t, "dir"), strings.Contains(t, "tree"), strings.Contains(t, "구조"):
		return "파일 확인 중"
	case strings.Contains(t, "draft"), strings.Contains(t, "answer"), strings.Contains(t, "정리"):
		return "답변 정리 중"
	default:
		return "분석 중"
	}
}

func classifyToolStatus(task string) string {
	t := strings.ToLower(strings.TrimSpace(task))
	switch {
	case strings.Contains(t, "test"), strings.Contains(t, "pytest"), strings.Contains(t, "go test"), strings.Contains(t, "cargo test"):
		return "테스트 실행 중"
	case strings.Contains(t, "read"), strings.Contains(t, "cat "), strings.Contains(t, "rg "), strings.Contains(t, "docs"), strings.Contains(t, "문서"):
		return "문서 읽는 중"
	case strings.Contains(t, "config"), strings.Contains(t, "env"), strings.Contains(t, "setting"), strings.Contains(t, "설정"):
		return "설정 확인 중"
	default:
		return "명령 실행 중"
	}
}

func buildInlineKeyboardForResponse(text string, svc Service, chatID int64) any {
	id := extractApprovalID(text)
	if id == "" {
		return nil
	}
	list, err := svc.MCP.ListApprovals(mcp.WithSessionKey(context.Background(), sessionKey(chatID)))
	if err != nil || len(list) == 0 {
		return nil
	}
	found := false
	for _, ap := range list {
		if ap.ID == id {
			found = true
			break
		}
	}
	if !found {
		return nil
	}
	approveBtn := inlineKeyboardButton{Text: "허용", CallbackData: approvalCallbackData(true, id)}
	denyBtn := inlineKeyboardButton{Text: "거절", CallbackData: approvalCallbackData(false, id)}
	rows := [][]inlineKeyboardButton{{approveBtn, denyBtn}}
	return inlineKeyboardMarkup{InlineKeyboard: rows}
}

func extractApprovalID(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ID: apr-") {
			return strings.TrimSpace(strings.TrimPrefix(line, "ID:"))
		}
	}
	return ""
}

func approvalCallbackData(approve bool, id string) string {
	if approve {
		return "apr:ok:" + id
	}
	return "apr:no:" + id
}

func handleCallback(cfg PollingConfig, chatID int64, data string) (string, error) {
	data = strings.TrimSpace(data)
	ctx := mcp.WithSessionKey(context.Background(), sessionKey(chatID))
	validateApprovalID := func(id string) (string, bool, error) {
		id = strings.TrimSpace(id)
		if id == "" {
			return "", false, nil
		}
		list, err := cfg.Handler.MCP.ListApprovals(ctx)
		if err != nil {
			return "", false, err
		}
		for _, ap := range list {
			if ap.ID == id {
				return id, true, nil
			}
		}
		return id, false, nil
	}
	switch {
	case strings.HasPrefix(data, "apr:"):
		parts := strings.SplitN(strings.TrimPrefix(data, "apr:"), ":", 2)
		if len(parts) != 2 {
			return "invalid approval action", nil
		}
		action := strings.TrimSpace(parts[0])
		id := strings.TrimSpace(parts[1])
		_, ok, err := validateApprovalID(id)
		if err != nil {
			return "승인 목록 조회 실패: " + err.Error(), err
		}
		if !ok {
			return "이미 처리되었거나 만료된 승인 요청입니다: " + id, nil
		}
		switch action {
		case "ok":
			return resolveApprovalDecision(cfg.Handler.MCP, ctx, id, "accept")
		case "no":
			return resolveApprovalDecision(cfg.Handler.MCP, ctx, id, "decline")
		default:
			return "invalid approval action", nil
		}
	case strings.HasPrefix(data, "approval:"):
		parts := strings.SplitN(strings.TrimPrefix(data, "approval:"), ":", 2)
		if len(parts) != 2 {
			return "invalid approval action", nil
		}
		action := strings.TrimSpace(parts[0])
		id := strings.TrimSpace(parts[1])
		_, ok, err := validateApprovalID(id)
		if err != nil {
			return "승인 목록 조회 실패: " + err.Error(), err
		}
		if !ok {
			return "이미 처리되었거나 만료된 승인 요청입니다: " + id, nil
		}
		switch action {
		case "1":
			return resolveApprovalDecision(cfg.Handler.MCP, ctx, id, "accept")
		case "2":
			return resolveApprovalDecision(cfg.Handler.MCP, ctx, id, "acceptForSession")
		case "3":
			return resolveApprovalDecision(cfg.Handler.MCP, ctx, id, "decline")
		case "4":
			return resolveApprovalDecision(cfg.Handler.MCP, ctx, id, "cancel")
		default:
			return "invalid approval action", nil
		}
	case strings.HasPrefix(data, "approve:"):
		id := strings.TrimSpace(strings.TrimPrefix(data, "approve:"))
		_, ok, err := validateApprovalID(id)
		if err != nil {
			return "승인 목록 조회 실패: " + err.Error(), err
		}
		if !ok {
			return "이미 처리되었거나 만료된 승인 요청입니다: " + id, nil
		}
		return mcp.ResolveApprovalBool(cfg.Handler.MCP, ctx, id, true)
	case strings.HasPrefix(data, "deny:"):
		id := strings.TrimSpace(strings.TrimPrefix(data, "deny:"))
		_, ok, err := validateApprovalID(id)
		if err != nil {
			return "승인 목록 조회 실패: " + err.Error(), err
		}
		if !ok {
			return "이미 처리되었거나 만료된 승인 요청입니다: " + id, nil
		}
		return mcp.ResolveApprovalBool(cfg.Handler.MCP, ctx, id, false)
	default:
		return "unknown callback", nil
	}
}

func approvalWaitMessage(svc Service, chatID int64) (string, bool) {
	approvals, err := svc.MCP.ListApprovals(mcp.WithSessionKey(context.Background(), sessionKey(chatID)))
	if err != nil || len(approvals) == 0 {
		return "", false
	}
	var b strings.Builder
	b.WriteString("승인 대기 중입니다.\n")
	max := len(approvals)
	if max > 5 {
		max = 5
	}
	for i := 0; i < max; i++ {
		b.WriteString("- ")
		b.WriteString(approvals[i].ID)
		b.WriteString("\n")
	}
	if len(approvals) > max {
		b.WriteString(fmt.Sprintf("- 외 %d건\n", len(approvals)-max))
	}
	b.WriteString("`/approve <id>` 또는 `/deny <id>`로 처리하세요.")
	return b.String(), true
}

func sessionKey(chatID int64) string {
	return fmt.Sprintf("tg:chat:%d", chatID)
}

func isReplyMarkupError(err error) bool {
	var apiErr *telegramAPIError
	if !asTelegramAPIError(err, &apiErr) || apiErr == nil {
		return false
	}
	msg := strings.ToLower(apiErr.Desc)
	return strings.Contains(msg, "reply markup") || strings.Contains(msg, "reply_markup")
}

func asTelegramAPIError(err error, target **telegramAPIError) bool {
	if err == nil || target == nil {
		return false
	}
	if v, ok := err.(*telegramAPIError); ok {
		*target = v
		return true
	}
	return false
}

func sanitizeTelegramText(s string) string {
	s = ansiEscapeRE.ReplaceAllString(s, "")
	s = ansiOrphanSGRRE.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\u0000", "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.TrimSpace(s)
	if s == "" {
		return " "
	}
	return s
}
