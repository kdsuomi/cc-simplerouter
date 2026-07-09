package simplerouter

import (
	"strings"
	"testing"
)

func TestSplitThinkingTail(t *testing.T) {
	tests := []struct {
		name     string
		buf      string
		wantEmit string
		wantTail string
	}{
		{name: "empty", buf: "", wantEmit: "", wantTail: ""},
		{name: "no boundary held back", buf: "alpha", wantEmit: "", wantTail: "alpha"},
		{name: "cuts after last space", buf: "alpha beta gam", wantEmit: "alpha beta ", wantTail: "gam"},
		{name: "prefers newline over space", buf: "alpha beta\ngamma del", wantEmit: "alpha beta\n", wantTail: "gamma del"},
		{name: "trailing space emits all", buf: "alpha beta ", wantEmit: "alpha beta ", wantTail: ""},
		{
			name:     "boundary-free run emits whole",
			buf:      strings.Repeat("x", thinkingMaxTailChars+1),
			wantEmit: strings.Repeat("x", thinkingMaxTailChars+1),
			wantTail: "",
		},
		{
			name:     "oversized tail after boundary emits whole",
			buf:      "a " + strings.Repeat("x", thinkingMaxTailChars+1),
			wantEmit: "a " + strings.Repeat("x", thinkingMaxTailChars+1),
			wantTail: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			emit, tail := splitThinkingTail(tt.buf)
			if emit != tt.wantEmit || tail != tt.wantTail {
				t.Fatalf("splitThinkingTail(%q) = (%q, %q), want (%q, %q)", tt.buf, emit, tail, tt.wantEmit, tt.wantTail)
			}
			if emit+tail != tt.buf {
				t.Fatalf("split loses text: %q + %q != %q", emit, tail, tt.buf)
			}
		})
	}
}

func TestJoinThinkingChunks(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
		want   string
	}{
		{name: "empty", chunks: nil, want: ""},
		{name: "single", chunks: []string{"alpha"}, want: "alpha"},
		{name: "rotated blocks concatenate verbatim", chunks: []string{"alpha beta ", "gamma"}, want: "alpha beta gamma"},
		{name: "newline boundary concatenates verbatim", chunks: []string{"alpha\n", "beta"}, want: "alpha\nbeta"},
		{name: "unrelated thoughts get a paragraph break", chunks: []string{"alpha", "beta"}, want: "alpha\n\nbeta"},
		{name: "skips empty chunks", chunks: []string{"", "alpha ", "", "beta"}, want: "alpha beta"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := joinThinkingChunks(tt.chunks); got != tt.want {
				t.Fatalf("joinThinkingChunks(%q) = %q, want %q", tt.chunks, got, tt.want)
			}
		})
	}
}
