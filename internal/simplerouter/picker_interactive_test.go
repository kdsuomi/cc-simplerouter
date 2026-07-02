package simplerouter

import (
	"fmt"
	"testing"
)

func interactiveTestModels(n int) []Model {
	models := make([]Model, n)
	for i := range models {
		models[i] = Model{ID: fmt.Sprintf("vendor/model-%03d", i), Name: fmt.Sprintf("Model %03d", i)}
	}
	return models
}

func TestPickerStateArrowNavigation(t *testing.T) {
	st := newPickerState(interactiveTestModels(30))
	if st.cursor != 0 || st.page() != 0 {
		t.Fatalf("initial cursor=%d page=%d", st.cursor, st.page())
	}

	// Down arrow past the bottom of page 1 moves onto page 2.
	for i := 0; i < pickerPageSize; i++ {
		st.handleInput([]byte("\x1b[B"))
	}
	if st.cursor != pickerPageSize || st.page() != 1 {
		t.Fatalf("after %d downs: cursor=%d page=%d", pickerPageSize, st.cursor, st.page())
	}

	// Up arrow clamps at the top.
	for i := 0; i < 100; i++ {
		st.handleInput([]byte("\x1b[A"))
	}
	if st.cursor != 0 {
		t.Fatalf("cursor should clamp to 0, got %d", st.cursor)
	}

	// Right/left arrows page and clamp.
	st.handleInput([]byte("\x1b[C"))
	if st.page() != 1 || st.cursor != pickerPageSize {
		t.Fatalf("right: page=%d cursor=%d", st.page(), st.cursor)
	}
	for i := 0; i < 10; i++ {
		st.handleInput([]byte("\x1b[C"))
	}
	if st.page() != st.totalPages()-1 {
		t.Fatalf("right should clamp to last page %d, got %d", st.totalPages()-1, st.page())
	}
	st.handleInput([]byte("\x1b[D"))
	st.handleInput([]byte("\x1b[D"))
	if st.page() != st.totalPages()-3 {
		t.Fatalf("left paging wrong: page=%d", st.page())
	}
}

func TestPickerStateTypingFiltersAndSelects(t *testing.T) {
	models := append(interactiveTestModels(20),
		Model{ID: "z-ai/glm-5.2", Name: "GLM 5.2"},
	)
	st := newPickerState(models)

	// Typing (including digits) goes into the search box, not row selection.
	if act := st.handleInput([]byte("glm")); act != pickerNone {
		t.Fatalf("typing returned action %v", act)
	}
	if st.query != "glm" {
		t.Fatalf("query=%q", st.query)
	}
	if len(st.filtered) != 1 || st.filtered[0].ID != "z-ai/glm-5.2" {
		t.Fatalf("filtered=%v", st.filtered)
	}

	sel, ok := st.selected()
	if !ok || sel.ID != "z-ai/glm-5.2" {
		t.Fatalf("selected=%v ok=%v", sel, ok)
	}
	if act := st.handleInput([]byte("\r")); act != pickerSelect {
		t.Fatalf("enter returned %v", act)
	}

	// Backspace widens the result set again.
	st.handleInput([]byte{0x7f})
	if st.query != "gl" {
		t.Fatalf("after backspace query=%q", st.query)
	}

	// Ctrl+U clears the box entirely.
	st.handleInput([]byte{0x15})
	if st.query != "" || len(st.filtered) != len(models) {
		t.Fatalf("after ctrl+u query=%q filtered=%d", st.query, len(st.filtered))
	}
}

func TestPickerStateDigitsAreSearchNotSelection(t *testing.T) {
	models := []Model{
		{ID: "vendor/model-1", Name: "One"},
		{ID: "vendor/model-2", Name: "Two"},
		{ID: "other/thing-12", Name: "Twelve"},
	}
	st := newPickerState(models)
	st.handleInput([]byte("12"))
	if st.query != "12" {
		t.Fatalf("query=%q", st.query)
	}
	if len(st.filtered) != 1 || st.filtered[0].ID != "other/thing-12" {
		t.Fatalf("digit typing should filter, got %v", st.filtered)
	}
}

func TestPickerStateStartsAtTopEscBacksCtrlCQuits(t *testing.T) {
	st := newPickerState(interactiveTestModels(300))
	if st.cursor != 0 || st.page() != 0 {
		t.Fatalf("picker should start at the top: cursor=%d page=%d", st.cursor, st.page())
	}
	if act := st.handleInput([]byte{0x1b}); act != pickerBack {
		t.Fatalf("bare ESC should go back to the provider picker, got %v", act)
	}
	if act := st.handleInput([]byte{0x03}); act != pickerQuit {
		t.Fatalf("Ctrl+C should quit, got %v", act)
	}
	// Arrow keys (ESC-prefixed CSI sequences) must not trigger back.
	if act := st.handleInput([]byte("\x1b[B")); act != pickerNone {
		t.Fatalf("down arrow = %v, want none", act)
	}
}

func TestPickerStateTabOpensProviders(t *testing.T) {
	st := newPickerState(interactiveTestModels(5))
	// Tab (0x09) opens the provider view.
	if act := st.handleInput([]byte{0x09}); act != pickerProviders {
		t.Fatalf("Tab = %v, want providers", act)
	}
	// Tab opens providers even while a filter is being typed (it is a
	// control byte, so it never collides with search text).
	st.handleInput([]byte("g"))
	if act := st.handleInput([]byte{0x09}); act != pickerProviders {
		t.Fatalf("Tab mid-filter = %v, want providers", act)
	}
	if st.query != "g" {
		t.Fatalf("query = %q, want g", st.query)
	}
}

func TestProviderStateNavigationAndActions(t *testing.T) {
	eps := make([]Endpoint, 20)
	for i := range eps {
		eps[i] = Endpoint{ProviderName: fmt.Sprintf("P%02d", i), Tag: fmt.Sprintf("p%02d/fp8", i)}
	}
	p := newProviderState(Model{ID: "a/b"}, eps, nil)
	if p.cursor != 0 || p.page() != 0 {
		t.Fatalf("initial cursor=%d page=%d", p.cursor, p.page())
	}
	for i := 0; i < pickerPageSize; i++ {
		p.handleInput([]byte("\x1b[B")) // down past page 1
	}
	if p.page() != 1 {
		t.Fatalf("after %d downs page=%d, want 1", pickerPageSize, p.page())
	}
	for i := 0; i < 50; i++ {
		p.handleInput([]byte("\x1b[A")) // up clamps to top
	}
	if p.cursor != 0 {
		t.Fatalf("cursor should clamp to 0, got %d", p.cursor)
	}

	if act := p.handleInput([]byte("\r")); act != pickerSelect {
		t.Fatalf("enter = %v, want select", act)
	}
	if ep, ok := p.selected(); !ok || ep.Tag != "p00/fp8" {
		t.Fatalf("selected = %+v ok=%v", ep, ok)
	}
	if act := p.handleInput([]byte{0x1b}); act != pickerBack {
		t.Fatalf("bare ESC = %v, want back", act)
	}
	if act := p.handleInput([]byte{0x03}); act != pickerQuit {
		t.Fatalf("Ctrl+C = %v, want quit", act)
	}
}

func TestRenderInteractiveHasConstantHeight(t *testing.T) {
	a := &app{}
	style := terminalStyle{enabled: true}

	full, _, _ := a.renderModelView(newPickerState(interactiveTestModels(40)), style)

	stEmpty := newPickerState(interactiveTestModels(40))
	stEmpty.handleInput([]byte("zzzzz")) // matches nothing
	empty, _, _ := a.renderModelView(stEmpty, style)

	stLast := newPickerState(interactiveTestModels(40))
	stLast.handleInput([]byte{0x1b, '[', 'F'}) // jump to end → short last page
	last, _, _ := a.renderModelView(stLast, style)

	if len(full) != len(empty) || len(full) != len(last) {
		t.Fatalf("inconsistent block heights: full=%d empty=%d last=%d", len(full), len(empty), len(last))
	}

	// Provider view must share the same fixed height for stable in-place redraw.
	pv := a.renderProviderView(newProviderState(Model{ID: "z-ai/glm-5.2"}, []Endpoint{
		{ProviderName: "DeepInfra", Tag: "deepinfra/fp4", Quantization: "fp4", ContextLength: 1048576, OutputPrice: "0.000003"},
	}, nil), style)
	if len(pv) != len(full) {
		t.Fatalf("provider view height %d != model view height %d", len(pv), len(full))
	}
}
