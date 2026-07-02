package simplerouter

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
)

// geminiDummySignature is Google's documented placeholder that skips thought
// signature validation (base64 of "skip_thought_signature_validator"). Used
// when a real signature is unavailable, e.g. history from a resumed session.
const geminiDummySignature = "c2tpcF90aG91Z2h0X3NpZ25hdHVyZV92YWxpZGF0b3I="

// toolCallRecord remembers what Gemini issued for one tool_use id. signature
// may legitimately be "" — in a parallel call batch Gemini attaches the
// signature to the first functionCall only, and re-adding one to the others
// on replay would violate that invariant.
type toolCallRecord struct {
	name      string
	signature string
}

// signatureStore maps generated tool_use ids to Gemini functionCall metadata.
// Claude Code cannot round-trip the thoughtSignature on tool_use blocks, so
// the proxy keeps it here for the lifetime of the session.
type signatureStore struct {
	mu sync.Mutex
	m  map[string]toolCallRecord
}

func newSignatureStore() *signatureStore {
	return &signatureStore{m: map[string]toolCallRecord{}}
}

func (s *signatureStore) record(id, name, sig string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[id] = toolCallRecord{name: name, signature: sig}
}

// latchSignature fills in a signature that streamed in after the functionCall
// itself; it never overwrites one already captured.
func (s *signatureStore) latchSignature(id, sig string) {
	if id == "" || sig == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.m[id]
	if !ok || rec.signature != "" {
		return
	}
	rec.signature = sig
	s.m[id] = rec
}

func (s *signatureStore) lookup(id string) (toolCallRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.m[id]
	return rec, ok
}

func newToolUseID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "toolu_fallback"
	}
	return "toolu_" + hex.EncodeToString(b[:])
}

func newMessageID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "msg_fallback"
	}
	return "msg_" + hex.EncodeToString(b[:])
}

// anthropicToGemini translates an Anthropic Messages request into a Gemini
// generateContent request body.
func anthropicToGemini(req *anthropicRequest, sigs *signatureStore) (*geminiRequest, error) {
	out := &geminiRequest{
		Contents:          convertMessages(req.Messages, sigs),
		SystemInstruction: systemInstructionFromRaw(req.System),
		Tools:             convertTools(req.Tools),
		ToolConfig:        convertToolChoice(req.ToolChoice),
		GenerationConfig:  convertGenerationConfig(req),
	}
	return out, nil
}

// systemInstructionFromRaw accepts the Anthropic system field as either a
// plain string or an array of text blocks (Claude Code sends blocks with
// cache_control, which the block struct drops).
func systemInstructionFromRaw(raw json.RawMessage) *geminiContent {
	blocks, err := blocksFromContent(raw)
	if err != nil {
		return nil
	}
	var texts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			texts = append(texts, b.Text)
		}
	}
	if len(texts) == 0 {
		return nil
	}
	return &geminiContent{Parts: []geminiPart{{Text: strings.Join(texts, "\n\n")}}}
}

// blocksFromContent decodes message content that is either a JSON string or
// an array of content blocks.
func blocksFromContent(raw json.RawMessage) ([]anthropicBlock, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		if s == "" {
			return nil, nil
		}
		return []anthropicBlock{{Type: "text", Text: s}}, nil
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}
	return blocks, nil
}

// convertMessages maps Anthropic messages to Gemini contents, merging
// consecutive same-role turns (Gemini expects alternating history) and
// dropping turns that translate to zero parts.
func convertMessages(msgs []anthropicMessage, sigs *signatureStore) []geminiContent {
	var out []geminiContent
	for _, msg := range msgs {
		role := "user"
		if msg.Role == "assistant" {
			role = "model"
		}
		blocks, err := blocksFromContent(msg.Content)
		if err != nil {
			continue
		}
		var parts []geminiPart
		for _, b := range blocks {
			parts = append(parts, convertBlock(b, sigs)...)
		}
		if len(parts) == 0 {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Parts = append(out[n-1].Parts, parts...)
			continue
		}
		out = append(out, geminiContent{Role: role, Parts: parts})
	}
	return out
}

func convertBlock(b anthropicBlock, sigs *signatureStore) []geminiPart {
	switch b.Type {
	case "text":
		if b.Text == "" {
			return nil
		}
		return []geminiPart{{Text: b.Text}}
	case "image":
		if b.Source == nil || b.Source.Type != "base64" {
			return nil
		}
		return []geminiPart{{InlineData: &geminiBlob{MimeType: b.Source.MediaType, Data: b.Source.Data}}}
	case "thinking":
		// Unsigned thinking text is useless to Gemini (it can't validate it)
		// and redacted_thinking has no equivalent; both are dropped below.
		if b.Thinking == "" || b.Signature == "" {
			return nil
		}
		return []geminiPart{{Text: b.Thinking, Thought: true, ThoughtSignature: sanitizeSignature(b.Signature)}}
	case "tool_use":
		args := b.Input
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		part := geminiPart{FunctionCall: &geminiFuncCall{Name: b.Name, Args: args}}
		if rec, ok := sigs.lookup(b.ID); ok {
			// Empty stored signature means this call was a non-first member of
			// a parallel batch: replay it without one.
			part.ThoughtSignature = rec.signature
		} else {
			part.ThoughtSignature = geminiDummySignature
		}
		return []geminiPart{part}
	case "tool_result":
		return convertToolResult(b, sigs)
	default:
		return nil
	}
}

// convertToolResult maps an Anthropic tool_result block to a functionResponse
// part (plus sibling inlineData parts for any image content). Gemini matches
// results to calls by function name, so the name is recovered from the store.
func convertToolResult(b anthropicBlock, sigs *signatureStore) []geminiPart {
	name := b.ToolUseID
	if rec, ok := sigs.lookup(b.ToolUseID); ok {
		name = rec.name
	}

	var texts []string
	var images []geminiPart
	if blocks, err := blocksFromContent(b.Content); err == nil {
		for _, cb := range blocks {
			switch cb.Type {
			case "text":
				texts = append(texts, cb.Text)
			case "image":
				if cb.Source != nil && cb.Source.Type == "base64" {
					images = append(images, geminiPart{InlineData: &geminiBlob{MimeType: cb.Source.MediaType, Data: cb.Source.Data}})
				}
			}
		}
	}
	text := strings.Join(texts, "\n")

	// functionResponse.response must be a JSON object: pass objects through,
	// wrap everything else.
	var response json.RawMessage
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") && json.Valid([]byte(trimmed)) {
		response = json.RawMessage(trimmed)
	} else {
		key := "result"
		if b.IsError {
			key = "error"
		}
		wrapped, err := json.Marshal(map[string]string{key: text})
		if err != nil {
			wrapped = []byte(`{}`)
		}
		response = wrapped
	}

	parts := []geminiPart{{FunctionResponse: &geminiFuncResp{Name: name, Response: response}}}
	return append(parts, images...)
}

// sanitizeSignature guards against forwarding a non-Gemini signature (e.g. an
// Anthropic "claude#..." signature after a cross-provider resume): Gemini
// requires valid base64 and 400s otherwise.
func sanitizeSignature(sig string) string {
	if strings.ContainsAny(sig, "#") {
		return geminiDummySignature
	}
	if _, err := base64.StdEncoding.DecodeString(sig); err != nil {
		return geminiDummySignature
	}
	return sig
}

func convertTools(tools []anthropicTool) []geminiTool {
	var decls []geminiFuncDecl
	for _, t := range tools {
		// Server tool stubs (type "web_search_20250305" etc.) have no schema
		// Gemini could use; skip them.
		if t.Type != "" && t.Type != "custom" {
			continue
		}
		decls = append(decls, geminiFuncDecl{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  scrubJSONSchema(t.InputSchema),
		})
	}
	if len(decls) == 0 {
		return nil
	}
	return []geminiTool{{FunctionDeclarations: decls}}
}

func convertToolChoice(tc *anthropicToolChoice) *geminiToolConfig {
	if tc == nil {
		return nil
	}
	cfg := geminiFuncCallingConfig{}
	switch tc.Type {
	case "auto":
		cfg.Mode = "AUTO"
	case "any":
		cfg.Mode = "ANY"
	case "none":
		cfg.Mode = "NONE"
	case "tool":
		cfg.Mode = "ANY"
		cfg.AllowedFunctionNames = []string{tc.Name}
	default:
		return nil
	}
	return &geminiToolConfig{FunctionCallingConfig: cfg}
}

// geminiThinkingLevelBudget is the Anthropic budget_tokens boundary between
// thinkingLevel "low" and "high" on gemini-3+ models (which use levels, not
// token budgets).
const geminiThinkingLevelBudget = 8192

func convertGenerationConfig(req *anthropicRequest) *geminiGenConfig {
	cfg := &geminiGenConfig{
		Temperature:     req.Temperature,
		TopP:            req.TopP,
		MaxOutputTokens: req.MaxTokens,
		StopSequences:   req.StopSequences,
	}
	// Thinking disabled or absent: omit thinkingConfig entirely (a zero budget
	// is rejected by 2.5 Pro and gemini-3, which cannot disable thinking).
	if req.Thinking != nil && req.Thinking.Type == "enabled" {
		tc := &geminiThinkingConfig{IncludeThoughts: true}
		if strings.Contains(req.Model, "gemini-3") {
			tc.ThinkingLevel = "low"
			if req.Thinking.BudgetTokens > geminiThinkingLevelBudget {
				tc.ThinkingLevel = "high"
			}
		} else {
			budget := req.Thinking.BudgetTokens
			tc.ThinkingBudget = &budget
		}
		cfg.ThinkingConfig = tc
	}
	return cfg
}

// geminiToAnthropic translates a non-streaming Gemini response into an
// Anthropic message, recording tool_use ids in the signature store.
func geminiToAnthropic(resp *geminiResponse, model string, sigs *signatureStore, newID func() string) *anthropicMessageResponse {
	out := &anthropicMessageResponse{
		ID:      newMessageID(),
		Type:    "message",
		Role:    "assistant",
		Model:   model,
		Content: []map[string]any{},
	}
	var finish string
	sawToolUse := false
	if len(resp.Candidates) > 0 {
		cand := resp.Candidates[0]
		finish = cand.FinishReason
		for _, part := range cand.Content.Parts {
			switch {
			case part.FunctionCall != nil:
				id := newID()
				sigs.record(id, part.FunctionCall.Name, part.ThoughtSignature)
				args := part.FunctionCall.Args
				if len(args) == 0 {
					args = json.RawMessage(`{}`)
				}
				out.Content = append(out.Content, map[string]any{
					"type":  "tool_use",
					"id":    id,
					"name":  part.FunctionCall.Name,
					"input": args,
				})
				sawToolUse = true
			case part.Thought:
				out.Content = append(out.Content, map[string]any{
					"type":      "thinking",
					"thinking":  part.Text,
					"signature": part.ThoughtSignature,
				})
			case part.Text != "":
				out.Content = append(out.Content, map[string]any{
					"type": "text",
					"text": part.Text,
				})
			}
		}
	}
	out.StopReason = anthropicStopReason(finish, sawToolUse)
	if resp.UsageMetadata != nil {
		out.Usage = anthropicUsage{
			InputTokens:  resp.UsageMetadata.PromptTokenCount,
			OutputTokens: resp.UsageMetadata.CandidatesTokenCount + resp.UsageMetadata.ThoughtsTokenCount,
		}
	}
	return out
}

// anthropicStopReason maps a Gemini finishReason. Gemini reports STOP even
// when the turn ended in function calls, so tool use takes precedence.
func anthropicStopReason(finish string, sawToolUse bool) string {
	if sawToolUse {
		return "tool_use"
	}
	switch finish {
	case "MAX_TOKENS":
		return "max_tokens"
	default:
		return "end_turn"
	}
}
