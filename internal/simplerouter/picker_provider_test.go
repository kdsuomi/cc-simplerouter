package simplerouter

import (
	"strings"
	"testing"
)

func TestOptionStateNavigation(t *testing.T) {
	st := &optionState{opts: providerOptions()}
	up := []byte{0x1b, '[', 'A'}
	down := []byte{0x1b, '[', 'B'}

	if st.handleInput(up) != pickerNone || st.cursor != 0 {
		t.Errorf("up at top should clamp, cursor = %d", st.cursor)
	}
	if st.handleInput(down) != pickerNone || st.cursor != 1 {
		t.Errorf("down should move, cursor = %d", st.cursor)
	}
	for range providerOptions() {
		if st.handleInput(down) != pickerNone {
			t.Error("down should keep browsing")
		}
	}
	if st.cursor != len(providerOptions())-1 {
		t.Errorf("down at bottom should clamp, cursor = %d", st.cursor)
	}
	if st.handleInput([]byte{'\r'}) != pickerSelect {
		t.Error("Enter should select")
	}
	if st.handleInput([]byte{0x1b}) != pickerQuit {
		t.Error("bare ESC should quit")
	}
	if st.handleInput([]byte{0x03}) != pickerQuit {
		t.Error("Ctrl+C should quit")
	}
}

func TestPickOneLineMode(t *testing.T) {
	newApp := func(stdin string) (*app, *strings.Builder) {
		out := &strings.Builder{}
		return &app{stdin: strings.NewReader(stdin), stdout: out, stderr: out}, out
	}

	// "2" selects the second option.
	a, out := newApp("2\n")
	opt, err := a.pickOne("Select a provider", providerOptions(), "")
	if err != nil {
		t.Fatal(err)
	}
	if opt.ID != providerGemini {
		t.Errorf("picked %q", opt.ID)
	}
	if !strings.Contains(out.String(), "Select a provider") || !strings.Contains(out.String(), "Google AI Studio") {
		t.Errorf("output missing picker chrome:\n%s", out.String())
	}
	if strings.Contains(out.String(), "\x1b[") {
		t.Error("non-TTY output must not contain ANSI escapes")
	}

	// Plain Enter keeps the preselected (saved) option.
	a, _ = newApp("\n")
	opt, err = a.pickOne("Select a provider", providerOptions(), providerGemini)
	if err != nil {
		t.Fatal(err)
	}
	if opt.ID != providerGemini {
		t.Errorf("Enter should keep saved provider, got %q", opt.ID)
	}

	// Junk input warns and re-prompts; then a valid pick works.
	a, out = newApp("9\n1\n")
	opt, err = a.pickOne("Select a provider", providerOptions(), "")
	if err != nil {
		t.Fatal(err)
	}
	if opt.ID != providerOpenRouter {
		t.Errorf("picked %q", opt.ID)
	}
	if !strings.Contains(out.String(), "Choose a number") {
		t.Error("junk input should warn")
	}
}

func TestRenderOptionViewConstantHeight(t *testing.T) {
	style := terminalStyle{}
	st := &optionState{opts: providerOptions()}
	h0 := len(renderOptionView("Select a provider", st, style, false))
	st.cursor = 1
	h1 := len(renderOptionView("Select a provider", st, style, false))
	if h0 != h1 {
		t.Errorf("render height changed with cursor: %d vs %d", h0, h1)
	}
}
