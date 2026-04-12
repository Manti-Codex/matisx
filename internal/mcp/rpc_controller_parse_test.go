package mcp

import "testing"

func TestParseCodexMessageEventRawResponseItem(t *testing.T) {
	msg := map[string]interface{}{
		"type": "raw_response_item",
		"item": map[string]interface{}{
			"role": "assistant",
			"content": []interface{}{
				map[string]interface{}{"type": "output_text", "text": "hello"},
			},
		},
	}
	evs := parseCodexMessageEvent(msg)
	if len(evs) == 0 {
		t.Fatalf("expected events")
	}
	if evs[0].Kind != streamEventText || evs[0].Text != "hello" {
		t.Fatalf("unexpected first event: %#v", evs[0])
	}
}

func TestParseCodexMessageEventLegacyMessage(t *testing.T) {
	msg := map[string]interface{}{
		"type": "message",
		"role": "assistant",
		"content": []interface{}{
			map[string]interface{}{"type": "text", "text": "legacy"},
		},
	}
	evs := parseCodexMessageEvent(msg)
	if len(evs) == 0 {
		t.Fatalf("expected events")
	}
	if evs[0].Kind != streamEventText || evs[0].Text != "legacy" {
		t.Fatalf("unexpected event: %#v", evs[0])
	}
}

func TestParseEnvelopeRemoteError(t *testing.T) {
	env := map[string]interface{}{
		"error": map[string]interface{}{
			"code":    float64(-32601),
			"message": "tool not found",
			"data":    map[string]interface{}{"name": "codex"},
		},
	}
	remoteErr, ok := parseEnvelopeRemoteError(env, "tools/call")
	if !ok || remoteErr == nil {
		t.Fatalf("expected remote error")
	}
	if remoteErr.Code != -32601 {
		t.Fatalf("unexpected code: %d", remoteErr.Code)
	}
	if remoteErr.Method != "tools/call" {
		t.Fatalf("unexpected method: %s", remoteErr.Method)
	}
}
