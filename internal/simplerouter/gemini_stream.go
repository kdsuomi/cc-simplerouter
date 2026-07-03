package simplerouter

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// errStreamAborted stops readGeminiSSE after a mid-stream upstream error has
// already been translated into an SSE error event.
var errStreamAborted = errors.New("gemini stream aborted")

// readGeminiSSE consumes a Gemini streamGenerateContent?alt=sse body and
// invokes emit for each decoded event. bufio.Reader (not Scanner) because
// data lines can exceed Scanner's token limit.
func readGeminiSSE(body io.Reader, emit func(*geminiResponse) error) error {
	reader := bufio.NewReader(body)
	var data strings.Builder
	flush := func() error {
		if data.Len() == 0 {
			return nil
		}
		payload := data.String()
		data.Reset()
		var resp geminiResponse
		if err := json.Unmarshal([]byte(payload), &resp); err != nil {
			return fmt.Errorf("malformed gemini SSE payload: %w", err)
		}
		return emit(&resp)
	}
	for {
		line, err := reader.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if ferr := flush(); ferr != nil {
				return ferr
			}
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
		if err != nil {
			if ferr := flush(); ferr != nil {
				return ferr
			}
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// sseWriter emits Anthropic-style SSE events, flushing after each one.
type sseWriter struct {
	w     io.Writer
	flush http.Flusher
}

func (s *sseWriter) event(name string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	io.WriteString(s.w, "event: "+name+"\ndata: ")
	s.w.Write(data)
	io.WriteString(s.w, "\n\n")
	if s.flush != nil {
		s.flush.Flush()
	}
}

// geminiStreamTranslator converts a stream of Gemini responses into Anthropic
// Messages SSE events, tracking content-block transitions and latching thought
// signatures that may appear on one chunk and vanish from later ones.
type geminiStreamTranslator struct {
	out   *sseWriter
	sigs  *signatureStore
	newID func() string
	model string
	msgID string

	started    bool
	blockIndex int
	blockOpen  bool
	blockType  string // "thinking" | "text"
	pendingSig string // latched signature for the open thinking block
	lastToolID string // most recent tool_use id, for late signature latch

	sawToolUse bool
	finish     string
	usage      geminiUsage
	hasUsage   bool
	errored    bool
}

func newGeminiStreamTranslator(w io.Writer, flush http.Flusher, model string, sigs *signatureStore, newID func() string) *geminiStreamTranslator {
	return &geminiStreamTranslator{
		out:   &sseWriter{w: w, flush: flush},
		sigs:  sigs,
		newID: newID,
		model: model,
		msgID: newMessageID(),
	}
}

func (t *geminiStreamTranslator) onChunk(resp *geminiResponse) error {
	if resp.Error != nil {
		t.emitError(resp.Error)
		return errStreamAborted
	}
	t.start(resp)
	if resp.UsageMetadata != nil {
		t.usage = *resp.UsageMetadata
		t.hasUsage = true
	}
	if len(resp.Candidates) == 0 {
		return nil
	}
	cand := resp.Candidates[0]
	if cand.FinishReason != "" {
		t.finish = cand.FinishReason
	}
	for _, part := range cand.Content.Parts {
		switch {
		case part.FunctionCall != nil:
			t.onFunctionCall(part)
		case part.Thought:
			t.onThought(part)
		case part.Text != "":
			t.onText(part)
		case part.ThoughtSignature != "":
			// Bare late signature with no data field: attach to whatever it
			// can still help — the open thinking block or the last tool call.
			t.latchSignature(part.ThoughtSignature)
		}
	}
	return nil
}

func (t *geminiStreamTranslator) start(resp *geminiResponse) {
	if t.started {
		return
	}
	t.started = true
	inputTokens := 0
	if resp.UsageMetadata != nil {
		inputTokens = resp.UsageMetadata.PromptTokenCount
	}
	t.out.event("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            t.msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         t.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]int{"input_tokens": inputTokens, "output_tokens": 0},
		},
	})
}

func (t *geminiStreamTranslator) onThought(part geminiPart) {
	t.openBlock("thinking")
	t.out.event("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": t.blockIndex,
		"delta": map[string]any{"type": "thinking_delta", "thinking": part.Text},
	})
	if part.ThoughtSignature != "" && t.pendingSig == "" {
		t.pendingSig = part.ThoughtSignature // latch first-seen, never clear
	}
}

func (t *geminiStreamTranslator) onText(part geminiPart) {
	t.openBlock("text")
	t.out.event("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": t.blockIndex,
		"delta": map[string]any{"type": "text_delta", "text": part.Text},
	})
	if part.ThoughtSignature != "" {
		// Gemini-3 text-part signatures have no Anthropic representation;
		// keep them replayable via the store when a tool call is pending.
		t.sigs.latchSignature(t.lastToolID, part.ThoughtSignature)
	}
}

func (t *geminiStreamTranslator) onFunctionCall(part geminiPart) {
	t.closeBlock()
	id := t.newID()
	t.sigs.record(id, part.FunctionCall.Name, part.ThoughtSignature)
	t.lastToolID = id
	t.sawToolUse = true

	args := part.FunctionCall.Args
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	t.out.event("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": t.blockIndex,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    id,
			"name":  part.FunctionCall.Name,
			"input": map[string]any{},
		},
	})
	// Gemini delivers functionCall parts whole, so one delta carries the full args.
	t.out.event("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": t.blockIndex,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": string(args)},
	})
	t.out.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": t.blockIndex})
	t.blockIndex++
}

func (t *geminiStreamTranslator) latchSignature(sig string) {
	if t.blockOpen && t.blockType == "thinking" && t.pendingSig == "" {
		t.pendingSig = sig
		return
	}
	t.sigs.latchSignature(t.lastToolID, sig)
}

// openBlock ensures a block of the wanted type is open, closing any block of
// a different type first.
func (t *geminiStreamTranslator) openBlock(blockType string) {
	if t.blockOpen && t.blockType == blockType {
		return
	}
	t.closeBlock()
	t.blockType = blockType
	t.blockOpen = true
	block := map[string]any{"type": blockType}
	if blockType == "thinking" {
		block["thinking"] = ""
		block["signature"] = ""
	} else {
		block["text"] = ""
	}
	t.out.event("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         t.blockIndex,
		"content_block": block,
	})
}

func (t *geminiStreamTranslator) closeBlock() {
	if !t.blockOpen {
		return
	}
	if t.blockType == "thinking" {
		// Anthropic ordering: thinking_deltas, then signature_delta, then stop.
		t.out.event("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": t.blockIndex,
			"delta": map[string]any{"type": "signature_delta", "signature": t.pendingSig},
		})
		t.pendingSig = ""
	}
	t.out.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": t.blockIndex})
	t.blockOpen = false
	t.blockIndex++
}

// finishStream emits the trailing message_delta/message_stop after upstream EOF.
func (t *geminiStreamTranslator) finishStream() {
	if t.errored {
		return
	}
	t.start(&geminiResponse{}) // degenerate empty stream still yields a valid message
	t.closeBlock()
	t.out.event("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   anthropicStopReason(t.finish, t.sawToolUse),
			"stop_sequence": nil,
		},
		"usage": map[string]int{
			"input_tokens":  t.usage.PromptTokenCount,
			"output_tokens": t.usage.CandidatesTokenCount + t.usage.ThoughtsTokenCount,
		},
	})
	t.out.event("message_stop", map[string]any{"type": "message_stop"})
}

func (t *geminiStreamTranslator) emitError(gerr *geminiError) {
	t.errored = true
	t.closeBlock()
	_, errType := anthropicErrorForStatus(gerr.Code)
	t.out.event("error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errType, "message": gerr.Message},
	})
}
