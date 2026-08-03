package simplerouter

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// OpenRouter's server-tools wrapper — engaged whenever a request carries a
// server-side tool such as web_search — re-emits the entire reasoning text of
// a response a second time: the response.reasoning_text.delta stream ends with
// a delta repeating everything streamed so far, and the reasoning_text.done,
// content_part.done, output_item.done, and response.completed artifacts carry
// the doubled string. Downstream that doubles Codex's live thinking display,
// the persisted rollout reasoning items, and the char-based generation-rate
// estimate. This filter restores the single copy; a clean upstream stream is
// forwarded byte-for-byte. Observed 2026-08-03 with moonshotai/kimi-k3 via
// Fireworks; reproducible with any /responses request that includes the
// web_search tool.

// A replay is only ever suppressed after it has matched this many characters
// of the accumulated text. Below this, repeated prefixes are treated as
// legitimate model output (models do repeat short phrases verbatim; they do
// not re-emit their entire accumulated reasoning).
const reasoningReplayMinLength = 32

// A partial replay interrupted by end-of-part is still dropped once it has
// matched this many characters; shorter partial matches are flushed unchanged.
const reasoningReplayPartialDropLength = 64

type reasoningReplayFilter struct {
	parts map[string]*reasoningReplayPart
}

type reasoningReplayPart struct {
	text      []byte
	pending   [][]byte
	replayPos int
}

func newReasoningReplayFilter() *reasoningReplayFilter {
	return &reasoningReplayFilter{parts: map[string]*reasoningReplayPart{}}
}

// processBlock inspects one SSE event block (all lines including the blank
// terminator) and returns the blocks to forward downstream, in order. Events
// the filter does not understand are forwarded untouched.
func (f *reasoningReplayFilter) processBlock(block []byte) [][]byte {
	payload, ok := ssePayload(block)
	if !ok {
		return [][]byte{block}
	}
	var event struct {
		Type         string          `json:"type"`
		ItemID       string          `json:"item_id"`
		ContentIndex *int            `json:"content_index"`
		SummaryIndex *int            `json:"summary_index"`
		Delta        json.RawMessage `json:"delta"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return [][]byte{block}
	}
	switch event.Type {
	case "response.reasoning_text.delta":
		return f.onDelta(reasoningPartKey(event.ItemID, "c", event.ContentIndex), block, payload, event.Delta)
	case "response.reasoning_summary_text.delta":
		return f.onDelta(reasoningPartKey(event.ItemID, "s", event.SummaryIndex), block, payload, event.Delta)
	case "response.reasoning_text.done":
		pre := f.finalizePart(reasoningPartKey(event.ItemID, "c", event.ContentIndex))
		return append(pre, halveTextField(block, payload))
	case "response.reasoning_summary_text.done":
		pre := f.finalizePart(reasoningPartKey(event.ItemID, "s", event.SummaryIndex))
		return append(pre, halveTextField(block, payload))
	case "response.content_part.done":
		pre := f.finalizePart(reasoningPartKey(event.ItemID, "c", event.ContentIndex))
		return append(pre, halveContentPart(block, payload))
	case "response.output_item.done":
		pre := f.finalizeItem(event.ItemID, payload)
		return append(pre, halveReasoningItemEvent(block, payload))
	case "response.completed", "response.incomplete", "response.failed":
		pre := f.finalizeAll()
		return append(pre, halveCompletedResponse(block, payload))
	}
	return [][]byte{block}
}

// finish flushes any suspected-replay events still buffered when the stream
// ends without a terminal event.
func (f *reasoningReplayFilter) finish() [][]byte {
	return f.finalizeAll()
}

func (f *reasoningReplayFilter) onDelta(key string, block, payload, rawDelta []byte) [][]byte {
	var delta string
	if json.Unmarshal(rawDelta, &delta) != nil || delta == "" {
		return [][]byte{block}
	}
	part := f.parts[key]
	if part == nil {
		part = &reasoningReplayPart{}
		f.parts[key] = part
	}

	if len(part.pending) > 0 {
		remaining := part.text[part.replayPos:]
		switch {
		case len(delta) <= len(remaining) && delta == string(remaining[:len(delta)]):
			part.pending = append(part.pending, block)
			part.replayPos += len(delta)
			if part.replayPos == len(part.text) {
				part.pending = nil
				part.replayPos = 0
			}
			return nil
		case len(delta) > len(remaining) && string(remaining) == delta[:len(remaining)]:
			// The delta finishes the replay and carries fresh text beyond it.
			fresh := delta[len(remaining):]
			part.pending = nil
			part.replayPos = 0
			part.text = append(part.text, fresh...)
			return [][]byte{rewriteDeltaBlock(block, payload, fresh)}
		default:
			return part.flush(block, delta)
		}
	}

	if len(part.text) >= reasoningReplayMinLength && matchesPrefix(part.text, delta) {
		if len(delta) >= len(part.text) {
			fresh := delta[len(part.text):]
			if fresh == "" {
				// The entire accumulated text replayed in one delta.
				return nil
			}
			part.text = append(part.text, fresh...)
			return [][]byte{rewriteDeltaBlock(block, payload, fresh)}
		}
		part.pending = [][]byte{block}
		part.replayPos = len(delta)
		return nil
	}

	part.text = append(part.text, delta...)
	return [][]byte{block}
}

func (part *reasoningReplayPart) flush(block []byte, delta string) [][]byte {
	out := part.pending
	part.text = append(part.text, part.text[:part.replayPos]...)
	part.text = append(part.text, delta...)
	part.pending = nil
	part.replayPos = 0
	return append(out, block)
}

func (f *reasoningReplayFilter) finalizePart(key string) [][]byte {
	part := f.parts[key]
	if part == nil || len(part.pending) == 0 {
		return nil
	}
	if part.replayPos >= reasoningReplayPartialDropLength {
		part.pending = nil
		part.replayPos = 0
		return nil
	}
	out := part.pending
	part.text = append(part.text, part.text[:part.replayPos]...)
	part.pending = nil
	part.replayPos = 0
	return out
}

func (f *reasoningReplayFilter) finalizeItem(itemID string, payload []byte) [][]byte {
	var event struct {
		Item struct {
			ID string `json:"id"`
		} `json:"item"`
	}
	_ = json.Unmarshal(payload, &event)
	id := event.Item.ID
	if id == "" {
		id = itemID
	}
	var out [][]byte
	for key := range f.parts {
		if id != "" && len(key) > len(id) && key[:len(id)] == id && key[len(id)] == '\x1f' {
			out = append(out, f.finalizePart(key)...)
		}
	}
	return out
}

func (f *reasoningReplayFilter) finalizeAll() [][]byte {
	var out [][]byte
	for key := range f.parts {
		out = append(out, f.finalizePart(key)...)
	}
	return out
}

func reasoningPartKey(itemID, kind string, index *int) string {
	n := 0
	if index != nil {
		n = *index
	}
	return itemID + "\x1f" + kind + strconv.Itoa(n)
}

func matchesPrefix(text []byte, delta string) bool {
	if len(delta) >= len(text) {
		return delta[:len(text)] == string(text)
	}
	return string(text[:len(delta)]) == delta
}

// halveDoubledString collapses a string made of two identical halves. The
// exact-half signature is the wrapper's; genuine text never satisfies it at
// the lengths involved.
func halveDoubledString(s string) (string, bool) {
	n := len(s)
	if n < 2 || n%2 != 0 {
		return s, false
	}
	if s[:n/2] == s[n/2:] {
		return s[:n/2], true
	}
	return s, false
}

func halveTextField(block, payload []byte) []byte {
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &fields) != nil {
		return block
	}
	var text string
	if json.Unmarshal(fields["text"], &text) != nil {
		return block
	}
	halved, changed := halveDoubledString(text)
	if !changed {
		return block
	}
	encoded, err := json.Marshal(halved)
	if err != nil {
		return block
	}
	fields["text"] = encoded
	return rebuildDataBlock(block, fields)
}

func halveContentPart(block, payload []byte) []byte {
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &fields) != nil {
		return block
	}
	part, changed := halveReasoningContentEntry(fields["part"])
	if !changed {
		return block
	}
	fields["part"] = part
	return rebuildDataBlock(block, fields)
}

func halveReasoningItemEvent(block, payload []byte) []byte {
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &fields) != nil {
		return block
	}
	item, changed := halveReasoningItem(fields["item"])
	if !changed {
		return block
	}
	fields["item"] = item
	return rebuildDataBlock(block, fields)
}

func halveCompletedResponse(block, payload []byte) []byte {
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &fields) != nil {
		return block
	}
	var response map[string]json.RawMessage
	if json.Unmarshal(fields["response"], &response) != nil {
		return block
	}
	var output []json.RawMessage
	if json.Unmarshal(response["output"], &output) != nil {
		return block
	}
	changedAny := false
	for i, raw := range output {
		item, changed := halveReasoningItem(raw)
		if changed {
			output[i] = item
			changedAny = true
		}
	}
	if !changedAny {
		return block
	}
	encodedOutput, err := json.Marshal(output)
	if err != nil {
		return block
	}
	response["output"] = encodedOutput
	encodedResponse, err := json.Marshal(response)
	if err != nil {
		return block
	}
	fields["response"] = encodedResponse
	return rebuildDataBlock(block, fields)
}

func halveReasoningItem(raw json.RawMessage) (json.RawMessage, bool) {
	var item map[string]json.RawMessage
	if json.Unmarshal(raw, &item) != nil {
		return raw, false
	}
	var itemType string
	if json.Unmarshal(item["type"], &itemType) != nil || itemType != "reasoning" {
		return raw, false
	}
	changedAny := false
	for _, field := range []string{"content", "summary"} {
		entriesRaw, present := item[field]
		if !present {
			continue
		}
		var entries []json.RawMessage
		if json.Unmarshal(entriesRaw, &entries) != nil {
			continue
		}
		changedField := false
		for i, entry := range entries {
			halved, changed := halveReasoningContentEntry(entry)
			if changed {
				entries[i] = halved
				changedField = true
			}
		}
		if !changedField {
			continue
		}
		encoded, err := json.Marshal(entries)
		if err != nil {
			continue
		}
		item[field] = encoded
		changedAny = true
	}
	if !changedAny {
		return raw, false
	}
	encoded, err := json.Marshal(item)
	if err != nil {
		return raw, false
	}
	return encoded, true
}

func halveReasoningContentEntry(raw json.RawMessage) (json.RawMessage, bool) {
	var entry map[string]json.RawMessage
	if json.Unmarshal(raw, &entry) != nil {
		return raw, false
	}
	var entryType string
	if json.Unmarshal(entry["type"], &entryType) != nil {
		return raw, false
	}
	switch entryType {
	case "reasoning_text", "summary_text", "text":
	default:
		return raw, false
	}
	var text string
	if json.Unmarshal(entry["text"], &text) != nil {
		return raw, false
	}
	halved, changed := halveDoubledString(text)
	if !changed {
		return raw, false
	}
	encoded, err := json.Marshal(halved)
	if err != nil {
		return raw, false
	}
	entry["text"] = encoded
	return rebuildJSONObject(entry), true
}

func rebuildJSONObject(fields map[string]json.RawMessage) json.RawMessage {
	encoded, err := json.Marshal(fields)
	if err != nil {
		return json.RawMessage("{}")
	}
	return encoded
}

func rewriteDeltaBlock(block, payload []byte, delta string) []byte {
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &fields) != nil {
		return block
	}
	encoded, err := json.Marshal(delta)
	if err != nil {
		return block
	}
	fields["delta"] = encoded
	return rebuildDataBlock(block, fields)
}

// ssePayload extracts the joined data payload of an SSE event block. It
// returns false for blocks with no data lines or the [DONE] sentinel.
func ssePayload(block []byte) ([]byte, bool) {
	var data [][]byte
	for _, line := range bytes.Split(block, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		value, found := bytes.CutPrefix(line, []byte("data:"))
		if !found {
			continue
		}
		data = append(data, bytes.TrimPrefix(value, []byte(" ")))
	}
	if len(data) == 0 {
		return nil, false
	}
	payload := bytes.Join(data, []byte("\n"))
	if !bytes.HasPrefix(bytes.TrimSpace(payload), []byte("{")) {
		return nil, false
	}
	return payload, true
}

// rebuildDataBlock re-emits an SSE block with its data payload replaced,
// preserving every non-data line (event names, ids, comments, terminator).
func rebuildDataBlock(block []byte, fields map[string]json.RawMessage) []byte {
	payload, err := json.Marshal(fields)
	if err != nil {
		return block
	}
	var out bytes.Buffer
	wroteData := false
	for _, line := range bytes.SplitAfter(block, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		trimmed := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			if !wroteData {
				out.WriteString("data: ")
				out.Write(payload)
				out.WriteString("\n")
				wroteData = true
			}
			continue
		}
		out.Write(line)
	}
	return out.Bytes()
}
