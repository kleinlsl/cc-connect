package acp

import (
	"encoding/json"
	"testing"
)

func usageUpdateParams(t *testing.T, size, used int) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"sessionId": "sess-1",
		"update": map[string]any{
			"sessionUpdate": "usage_update",
			"size":          size,
			"used":          used,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestACPSession_ContextUsage_NilBeforeAnySignal(t *testing.T) {
	s := &acpSession{}
	if got := s.GetContextUsage(); got != nil {
		t.Fatalf("expected nil usage before any signal, got %+v", got)
	}
}

func TestACPSession_UsageUpdate_SetsWindowAndUsed(t *testing.T) {
	s := &acpSession{}
	s.absorbUsageUpdate(usageUpdateParams(t, 200000, 40000))
	u := s.GetContextUsage()
	if u == nil {
		t.Fatal("expected usage after usage_update")
	}
	if u.ContextWindow != 200000 {
		t.Errorf("ContextWindow = %d, want 200000", u.ContextWindow)
	}
	if u.UsedTokens != 40000 {
		t.Errorf("UsedTokens = %d, want 40000", u.UsedTokens)
	}
}

func TestACPSession_UsageUpdate_IgnoresOtherKinds(t *testing.T) {
	s := &acpSession{}
	b, _ := json.Marshal(map[string]any{
		"update": map[string]any{"sessionUpdate": "agent_message_chunk", "size": 1, "used": 2},
	})
	s.absorbUsageUpdate(b)
	if got := s.GetContextUsage(); got != nil {
		t.Fatalf("non usage_update must be ignored, got %+v", got)
	}
}

func TestACPSession_PromptUsage_MergesOverWindow(t *testing.T) {
	s := &acpSession{}
	// Window/used arrive first via usage_update.
	s.absorbUsageUpdate(usageUpdateParams(t, 200000, 50000))
	// Per-turn token counts arrive on the session/prompt response.
	res, _ := json.Marshal(map[string]any{
		"stopReason": "end_turn",
		"usage": map[string]any{
			"inputTokens":       1200,
			"outputTokens":      300,
			"totalTokens":       1500,
			"thoughtTokens":     40,
			"cachedReadTokens":  48000,
			"cachedWriteTokens": 800,
		},
	})
	s.absorbPromptUsage(res)

	u := s.GetContextUsage()
	if u == nil {
		t.Fatal("expected merged usage")
	}
	if u.ContextWindow != 200000 {
		t.Errorf("ContextWindow must be preserved from usage_update, got %d", u.ContextWindow)
	}
	if u.UsedTokens != 50000 {
		t.Errorf("UsedTokens must be preserved from usage_update, got %d", u.UsedTokens)
	}
	if u.InputTokens != 1200 || u.OutputTokens != 300 || u.TotalTokens != 1500 {
		t.Errorf("per-turn tokens wrong: %+v", u)
	}
	if u.ReasoningOutputTokens != 40 {
		t.Errorf("ReasoningOutputTokens = %d, want 40", u.ReasoningOutputTokens)
	}
	if u.CachedInputTokens != 48000 || u.CacheCreationInputTokens != 800 {
		t.Errorf("cache tokens wrong: cr=%d cw=%d", u.CachedInputTokens, u.CacheCreationInputTokens)
	}
}

func TestACPSession_PromptUsage_UsedFallbackWithoutUpdate(t *testing.T) {
	s := &acpSession{}
	// OpenAI-style: inputTokens(1000) is the whole prompt; cachedReadTokens(900)
	// is a cached subset of it and must NOT be added on top when estimating used.
	res, _ := json.Marshal(map[string]any{
		"usage": map[string]any{"inputTokens": 1000, "cachedReadTokens": 900, "outputTokens": 10},
	})
	s.absorbPromptUsage(res)
	u := s.GetContextUsage()
	if u == nil {
		t.Fatal("expected usage")
	}
	if u.UsedTokens != 1000 {
		t.Errorf("UsedTokens fallback = %d, want 1000 (cached subset must not be double-counted)", u.UsedTokens)
	}
}

func TestACPSession_GetContextUsage_ReturnsClone(t *testing.T) {
	s := &acpSession{}
	s.absorbUsageUpdate(usageUpdateParams(t, 1000, 100))
	first := s.GetContextUsage()
	first.ContextWindow = 999
	second := s.GetContextUsage()
	if second.ContextWindow != 1000 {
		t.Errorf("mutating returned usage must not change session state, got %d", second.ContextWindow)
	}
}

func TestACPSession_GetModel_FromHandshakeBlock(t *testing.T) {
	s := &acpSession{}
	if got := s.GetModel(); got != "" {
		t.Errorf("model should start empty, got %q", got)
	}
	s.absorbModel(&acpModelBlock{CurrentModelID: "custom:new-api-01/glm-5.3-flash"})
	if got := s.GetModel(); got != "custom:new-api-01/glm-5.3-flash" {
		t.Errorf("GetModel = %q", got)
	}
	// Nil block must not clear the value.
	s.absorbModel(nil)
	if got := s.GetModel(); got != "custom:new-api-01/glm-5.3-flash" {
		t.Errorf("GetModel after nil block = %q", got)
	}
}
