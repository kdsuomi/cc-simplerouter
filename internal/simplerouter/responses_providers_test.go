package simplerouter

import "testing"

func TestDeepSeekResponsesOptions(t *testing.T) {
	got := deepSeekResponsesOptions(false)
	if got.ChatPath != "/chat/completions" || got.ReasoningReplayField != "reasoning_content" || !got.IncludeStreamUsage {
		t.Fatalf("DeepSeek options = %+v", got)
	}
	if got.ReasoningEffortMap["medium"] != "high" || got.ReasoningEffortMap["xhigh"] != "max" {
		t.Fatalf("DeepSeek effort map = %#v", got.ReasoningEffortMap)
	}
	thinking := got.ExtraBody["thinking"].(map[string]any)
	if thinking["type"] != "enabled" {
		t.Fatalf("DeepSeek thinking = %#v", thinking)
	}

	disabled := deepSeekResponsesOptions(true)
	if disabled.ExtraBody["thinking"].(map[string]any)["type"] != "disabled" {
		t.Fatalf("DeepSeek disabled thinking = %#v", disabled.ExtraBody)
	}
}

func TestZAIResponsesOptions(t *testing.T) {
	got := zaiResponsesOptions(false)
	if !got.ToolStream || got.IncludeStreamUsage || got.ReasoningEffortMap["ultra"] != "max" {
		t.Fatalf("Z.AI options = %+v", got)
	}
	thinking := got.ExtraBody["thinking"].(map[string]any)
	if thinking["type"] != "enabled" || thinking["clear_thinking"] != false {
		t.Fatalf("Z.AI thinking = %#v", thinking)
	}

	disabled := zaiResponsesOptions(true)
	if disabled.ExtraBody["thinking"].(map[string]any)["type"] != "disabled" {
		t.Fatalf("Z.AI disabled thinking = %#v", disabled.ExtraBody)
	}
}
