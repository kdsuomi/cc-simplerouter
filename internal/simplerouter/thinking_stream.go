package simplerouter

import (
	"strings"
	"time"
)

const (
	// thinkingRotateEvery is how often an open thinking block is closed and a
	// fresh one opened while reasoning is still streaming.
	thinkingRotateEvery = 500 * time.Millisecond
	// thinkingRotateMinChars keeps rotation from producing confetti when the
	// model reasons slowly: a block only rotates once it holds at least this
	// much text.
	thinkingRotateMinChars = 24
	// thinkingMaxTailChars caps how much text may be held back while waiting
	// for a whitespace boundary (reasoning without spaces, e.g. CJK).
	thinkingMaxTailChars = 160
)

// thinkingStreamer streams reasoning text as Anthropic thinking blocks in a
// hybrid form that renders progressively in every Claude Code build:
//
//  1. Deltas are forwarded immediately as thinking_delta events, which
//     patched binaries (prepareClaudeLiveThinkingPatch) render live.
//  2. The open thinking block is rotated (closed and reopened) on a cadence.
//     Claude Code's engine lands an assistant message for every completed
//     content block, so even unpatched builds show thinking advancing block
//     by block instead of only after the whole response.
//
// Rotation cuts at whitespace, and the cut whitespace stays inside the
// emitted text, so concatenating the rotated blocks reproduces the original
// reasoning exactly (see joinThinkingChunks).
//
// The host translator supplies its block bookkeeping through the callbacks;
// all methods must be called from the goroutine that owns the SSE writer.
type thinkingStreamer struct {
	open      func()       // ensure the current open block is a thinking block
	closeCurr func()       // close the currently open thinking block
	emit      func(string) // emit a thinking_delta into the open thinking block
	isOpen    func() bool  // whether a thinking block is currently open

	rotateEvery time.Duration
	minChars    int
	now         func() time.Time

	tail     string // held-back text waiting for a whitespace boundary
	openedAt time.Time
	emitted  int // characters emitted into the current block
}

// add streams a reasoning delta. A short partial-word tail is held back so
// that block rotation cuts on whitespace instead of mid-word.
func (s *thinkingStreamer) add(delta string) {
	if delta == "" {
		return
	}
	emitNow, tail := splitThinkingTail(s.tail + delta)
	s.tail = tail
	if emitNow == "" {
		return
	}
	if s.rotateDue() {
		s.closeCurr()
	}
	s.write(emitNow)
}

// flush emits any held-back tail, opening a thinking block if none is open.
// Hosts must call it before switching to another block type, finishing the
// message, or reporting an error.
func (s *thinkingStreamer) flush() {
	if s.tail == "" {
		return
	}
	text := s.tail
	s.tail = ""
	s.write(text)
}

func (s *thinkingStreamer) write(text string) {
	if !s.isOpen() {
		s.open()
		s.openedAt = s.now()
		s.emitted = 0
	}
	s.emit(text)
	s.emitted += len(text)
}

func (s *thinkingStreamer) rotateDue() bool {
	if s.rotateEvery <= 0 || !s.isOpen() || s.emitted < s.minChars {
		return false
	}
	return s.now().Sub(s.openedAt) >= s.rotateEvery
}

// splitThinkingTail splits buffered reasoning into text safe to emit now and
// a tail to hold until the next whitespace arrives. Newlines are preferred
// cut points, then spaces; a boundary-free run longer than
// thinkingMaxTailChars is emitted whole rather than buffered forever.
func splitThinkingTail(buf string) (emitNow, tail string) {
	cut := strings.LastIndexByte(buf, '\n')
	if cut < 0 {
		cut = strings.LastIndexByte(buf, ' ')
	}
	if cut < 0 {
		if len(buf) > thinkingMaxTailChars {
			return buf, ""
		}
		return "", buf
	}
	if len(buf)-(cut+1) > thinkingMaxTailChars {
		return buf, ""
	}
	return buf[:cut+1], buf[cut+1:]
}

// joinThinkingChunks reassembles thinking blocks that a thinkingStreamer (or
// the user's client) split apart. Chunks that already touch at whitespace are
// concatenated verbatim; anything else gets a paragraph break so unrelated
// thoughts don't run together.
func joinThinkingChunks(chunks []string) string {
	var b strings.Builder
	for _, chunk := range chunks {
		if chunk == "" {
			continue
		}
		if b.Len() > 0 && !endsWithWhitespace(b.String()) && !startsWithWhitespace(chunk) {
			b.WriteString("\n\n")
		}
		b.WriteString(chunk)
	}
	return b.String()
}

func endsWithWhitespace(s string) bool {
	return s != "" && isThinkingBoundary(s[len(s)-1])
}

func startsWithWhitespace(s string) bool {
	return s != "" && isThinkingBoundary(s[0])
}

func isThinkingBoundary(c byte) bool {
	return c == ' ' || c == '\n' || c == '\t' || c == '\r'
}
