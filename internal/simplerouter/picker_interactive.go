package simplerouter

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// errPickerFallback signals that the interactive picker could not start (raw
// mode unavailable, terminal too small, …) so the caller should use line mode.
var errPickerFallback = errors.New("picker: interactive mode unavailable")

var errPickerCancelled = errors.New("model selection cancelled")

// errPickerBack signals that the user backed out of the model picker to
// return to the provider picker.
var errPickerBack = errors.New("picker: back to provider selection")

// pickerTerminals reports whether both stdin and stderr are real terminals,
// returning their files so the interactive picker can drive them directly.
func (a *app) pickerTerminals() (in *os.File, out *os.File, ok bool) {
	inFile, ok1 := a.stdin.(*os.File)
	outFile, ok2 := a.stderr.(*os.File)
	if !ok1 || !ok2 {
		return nil, nil, false
	}
	if !term.IsTerminal(int(inFile.Fd())) || !term.IsTerminal(int(outFile.Fd())) {
		return nil, nil, false
	}
	return inFile, outFile, true
}

// pickerState holds the cursor/search state of the interactive picker. It is
// pure (no I/O) so its navigation logic can be unit tested without a terminal.
type pickerState struct {
	all          []Model // ordered full list
	query        string  // current search box contents
	filtered     []Model // all filtered by query
	cursor       int     // absolute index into filtered (the highlighted row)
	title        string  // picker heading, e.g. "Select a Gemini model"
	hasProviders bool    // whether the Tab providers view is available
}

func newPickerState(all []Model) *pickerState {
	st := &pickerState{all: all, title: "Select a model"}
	st.refilter()
	return st
}

func (st *pickerState) refilter() {
	st.filtered = filterModels(st.all, st.query)
	st.clamp()
}

func (st *pickerState) clamp() {
	if st.cursor > len(st.filtered)-1 {
		st.cursor = len(st.filtered) - 1
	}
	if st.cursor < 0 {
		st.cursor = 0
	}
}

func (st *pickerState) page() int {
	if len(st.filtered) == 0 {
		return 0
	}
	return st.cursor / pickerPageSize
}

func (st *pickerState) totalPages() int { return pageCount(len(st.filtered), pickerPageSize) }

func (st *pickerState) moveUp()   { st.cursor--; st.clamp() }
func (st *pickerState) moveDown() { st.cursor++; st.clamp() }

func (st *pickerState) gotoPage(p int) {
	if p < 0 {
		p = 0
	}
	if p > st.totalPages()-1 {
		p = st.totalPages() - 1
	}
	st.cursor = p * pickerPageSize
	st.clamp()
}

func (st *pickerState) pageLeft()  { st.gotoPage(st.page() - 1) }
func (st *pickerState) pageRight() { st.gotoPage(st.page() + 1) }
func (st *pickerState) home()      { st.cursor = 0; st.clamp() }
func (st *pickerState) end()       { st.cursor = len(st.filtered) - 1; st.clamp() }

func (st *pickerState) addRune(r rune) {
	st.query += string(r)
	st.cursor = 0
	st.refilter()
}

func (st *pickerState) backspace() {
	if st.query == "" {
		return
	}
	r := []rune(st.query)
	st.query = string(r[:len(r)-1])
	st.cursor = 0
	st.refilter()
}

func (st *pickerState) clearQuery() {
	if st.query != "" {
		st.query = ""
		st.cursor = 0
		st.refilter()
	}
}

func (st *pickerState) selected() (Model, bool) {
	if len(st.filtered) == 0 {
		return Model{}, false
	}
	return st.filtered[st.cursor], true
}

// marker renders the row gutter for the interactive picker: a pointer for the
// highlighted row, a faint bullet otherwise. No row numbers — navigation is by
// arrow key, so the numbers the line-mode picker uses for selection are gone.
func (s terminalStyle) marker(selected bool) string {
	glyph, code := "·", clrFaint
	if selected {
		glyph, code = "▸", clrAccentBold
	}
	if !s.enabled {
		if selected {
			glyph = ">"
		} else {
			glyph = "-"
		}
	}
	return s.paint(code, glyph+strings.Repeat(" ", wGutter-1))
}

type pickerAction int

const (
	pickerNone pickerAction = iota
	pickerSelect
	pickerQuit
	pickerProviders // open provider view for the highlighted model
	pickerBack      // leave the provider view, back to the model list
)

// handleInput applies a chunk of raw terminal bytes (one or more keypresses)
// and reports whether the user committed a selection, opened providers, or quit.
func (st *pickerState) handleInput(buf []byte) pickerAction {
	for i := 0; i < len(buf); i++ {
		b := buf[i]
		switch {
		case b == 0x1b: // ESC: either a CSI sequence (arrows etc.) or a bare ESC
			if i+1 < len(buf) && buf[i+1] == '[' {
				j := i + 2
				for j < len(buf) && (buf[j] < 0x40 || buf[j] > 0x7e) {
					j++
				}
				if j >= len(buf) {
					return pickerNone // incomplete sequence; ignore the tail
				}
				st.applyCSI(string(buf[i+2:j]), buf[j])
				i = j
			} else if i+1 >= len(buf) {
				// A lone ESC always backs out to the provider picker;
				// Ctrl+C quits.
				return pickerBack
			}
			// ESC followed by some other byte (Alt-combo): ignore.
		case b == '\r' || b == '\n':
			return pickerSelect
		case b == 0x7f || b == 0x08: // DEL / Backspace
			st.backspace()
		case b == 0x03: // Ctrl+C
			return pickerQuit
		case b == 0x15: // Ctrl+U clears the search box
			st.clearQuery()
		case b == 0x09: // Tab opens the provider view for the highlighted model
			return pickerProviders
		case b >= 0x20 && b < 0x7f: // printable: letters, digits, space, ?, …
			st.addRune(rune(b))
		}
	}
	return pickerNone
}

func (st *pickerState) applyCSI(params string, final byte) {
	switch final {
	case 'A':
		st.moveUp()
	case 'B':
		st.moveDown()
	case 'C':
		st.pageRight()
	case 'D':
		st.pageLeft()
	case 'H':
		st.home()
	case 'F':
		st.end()
	case '~':
		switch params {
		case "5": // PageUp
			st.pageLeft()
		case "6": // PageDown
			st.pageRight()
		case "1", "7":
			st.home()
		case "4", "8":
			st.end()
		}
	}
}

// providerState holds the cursor over a model's provider endpoints.
type providerState struct {
	model     Model
	endpoints []Endpoint
	err       error
	cursor    int
}

func newProviderState(m Model, endpoints []Endpoint, err error) *providerState {
	return &providerState{model: m, endpoints: endpoints, err: err}
}

func (p *providerState) page() int {
	if len(p.endpoints) == 0 {
		return 0
	}
	return p.cursor / pickerPageSize
}
func (p *providerState) totalPages() int { return pageCount(len(p.endpoints), pickerPageSize) }
func (p *providerState) clamp() {
	if p.cursor > len(p.endpoints)-1 {
		p.cursor = len(p.endpoints) - 1
	}
	if p.cursor < 0 {
		p.cursor = 0
	}
}
func (p *providerState) gotoPage(n int) {
	if n < 0 {
		n = 0
	}
	if n > p.totalPages()-1 {
		n = p.totalPages() - 1
	}
	p.cursor = n * pickerPageSize
	p.clamp()
}
func (p *providerState) selected() (Endpoint, bool) {
	if len(p.endpoints) == 0 {
		return Endpoint{}, false
	}
	return p.endpoints[p.cursor], true
}

func (p *providerState) handleInput(buf []byte) pickerAction {
	for i := 0; i < len(buf); i++ {
		b := buf[i]
		switch {
		case b == 0x1b:
			if i+1 < len(buf) && buf[i+1] == '[' {
				j := i + 2
				for j < len(buf) && (buf[j] < 0x40 || buf[j] > 0x7e) {
					j++
				}
				if j >= len(buf) {
					return pickerNone
				}
				switch buf[j] {
				case 'A':
					p.cursor--
				case 'B':
					p.cursor++
				case 'C':
					p.gotoPage(p.page() + 1)
				case 'D':
					p.gotoPage(p.page() - 1)
				case 'H':
					p.cursor = 0
				case 'F':
					p.cursor = len(p.endpoints) - 1
				}
				p.clamp()
				i = j
			} else if i+1 >= len(buf) {
				return pickerBack // bare ESC backs out to the model list
			}
		case b == '\r' || b == '\n':
			return pickerSelect
		case b == 0x03:
			return pickerQuit
		case b == 'q':
			return pickerBack
		}
	}
	return pickerNone
}

// pickModelInteractive runs the full-screen arrow-key picker. It returns
// errPickerFallback if raw mode can't be set up, so the caller degrades to
// the line-based prompt.
func (a *app) pickModelInteractive(in, out *os.File, title string, models []Model, endpoints endpointsFunc) (pickResult, error) {
	// Refuse on terminals too short to hold the block without scroll glitches.
	if _, h, err := term.GetSize(int(out.Fd())); err == nil && h > 0 && h < pickerPageSize+9 {
		return pickResult{}, errPickerFallback
	}

	fd := int(in.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return pickResult{}, errPickerFallback
	}
	defer term.Restore(fd, oldState)
	defer io.WriteString(out, "\x1b[0 q\x1b[?25h") // always restore cursor style/visibility
	enableTerminalVT(in.Fd(), out.Fd())            // Windows: deliver arrows as escape sequences

	style := newTerminalStyle(out)
	st := newPickerState(models)
	st.title = title
	st.hasProviders = endpoints != nil
	var prov *providerState // non-nil while the provider view is open
	buf := make([]byte, 32)

	prevRow, lastN := 0, 0
	finish := func(confirm string) {
		var b strings.Builder
		if down := (lastN - 1) - prevRow; down > 0 {
			fmt.Fprintf(&b, "\x1b[%dB", down)
		}
		b.WriteString("\r\x1b[0 q\x1b[?25h\r\n")
		if confirm != "" {
			b.WriteString(confirm + "\r\n")
		}
		io.WriteString(out, b.String())
	}

	for {
		var lines []string
		caretRow, caretCol := -1, 0
		if prov == nil {
			lines, caretRow, caretCol = a.renderModelView(st, style)
		} else {
			lines = a.renderProviderView(prov, style)
		}
		lastN = len(lines)
		drawBlockCursor(out, lines, &prevRow, caretRow, caretCol)

		n, err := in.Read(buf)
		if n > 0 {
			if prov == nil {
				switch st.handleInput(buf[:n]) {
				case pickerSelect:
					if sel, ok := st.selected(); ok {
						finish("  " + style.paint(clrAccentBold, "▸ selected ") + style.paint(clrModelHi, sel.ID))
						return pickResult{Model: sel}, nil
					}
				case pickerProviders:
					if sel, ok := st.selected(); ok && endpoints != nil {
						eps, ferr := endpoints(sel.ID)
						prov = newProviderState(sel, eps, ferr)
					}
				case pickerBack:
					finish("")
					return pickResult{}, errPickerBack
				case pickerQuit:
					finish("")
					return pickResult{}, errPickerCancelled
				}
			} else {
				switch prov.handleInput(buf[:n]) {
				case pickerSelect:
					if ep, ok := prov.selected(); ok {
						m := prov.model
						m.ContextLength = ep.ContextLength
						finish("  " + style.paint(clrAccentBold, "▸ selected ") + style.paint(clrModelHi, m.ID) +
							style.paint(clrDim, " via ") + style.paint(clrModelHi, ep.ProviderName))
						return pickResult{Model: m, ProviderTag: ep.Tag, ProviderName: ep.ProviderName}, nil
					}
				case pickerBack:
					prov = nil
				case pickerQuit:
					finish("")
					return pickResult{}, errPickerCancelled
				}
			}
		}
		if err != nil {
			finish("")
			return pickResult{}, errPickerCancelled
		}
	}
}

// drawBlockCursor repaints the picker block in place and positions the real
// terminal cursor. It tracks the cursor's current row within the block (prevRow)
// so the next repaint can return to the top regardless of where the caret landed.
// When caretRow >= 0 the cursor is parked there as a blinking bar (the search
// field); otherwise it is hidden (provider view).
func drawBlockCursor(w io.Writer, lines []string, prevRow *int, caretRow, caretCol int) {
	var b strings.Builder
	b.WriteString("\x1b[?25l") // hide while repainting to avoid flicker
	if *prevRow > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", *prevRow)
	}
	b.WriteString("\r\x1b[0J")
	b.WriteString(strings.Join(lines, "\r\n"))

	endRow := len(lines) - 1
	if caretRow >= 0 {
		if up := endRow - caretRow; up > 0 {
			fmt.Fprintf(&b, "\x1b[%dA", up)
		}
		b.WriteString("\r")
		if caretCol > 0 {
			fmt.Fprintf(&b, "\x1b[%dC", caretCol)
		}
		b.WriteString("\x1b[5 q\x1b[?25h") // blinking bar + show
		*prevRow = caretRow
	} else {
		*prevRow = endRow
	}
	io.WriteString(w, b.String())
}

const searchCaretCol = 11 // visible width of "  search ❯ "

// renderModelView builds the model list block plus the caret position (row/col)
// for the blinking search cursor. Height is constant so the redraw is stable.
func (a *app) renderModelView(st *pickerState, style terminalStyle) (lines []string, caretRow, caretCol int) {
	lines = make([]string, 0, pickerPageSize+9)
	lines = append(lines, "") // blank separator above the picker

	count := fmt.Sprintf("%d match", len(st.filtered))
	if len(st.filtered) != 1 {
		count += "es"
	}
	meta := fmt.Sprintf("%s · page %d/%d", count, st.page()+1, st.totalPages())
	lines = append(lines, fmt.Sprintf("%s%s   %s",
		style.paint(clrAccent, "▌ "),
		style.header(st.title),
		style.paint(clrDim, meta),
	))

	box := "  " + style.paint(clrDim, "search ") + style.paint(clrAccentBold, "❯ ")
	if st.query == "" {
		box += style.paint(clrFaint, "type to filter")
	} else {
		box += style.paint(clrHead, st.query)
	}
	caretRow = len(lines) // the search line we are about to append
	caretCol = searchCaretCol + len([]rune(st.query))
	lines = append(lines, box, "")

	headerPlain := rowLine(padRight("", wGutter),
		padRight("MODEL", wModel),
		padRight("NAME", wName),
		padRight("CTX", wCtx),
		padRight("PRICE/M", wPrice),
	)
	lines = append(lines, style.paint(clrDim, headerPlain))
	lines = append(lines, style.paint(clrFaint, strings.Repeat("─", len(strings.TrimRight(headerPlain, " "))+2)))

	rows := make([]string, 0, pickerPageSize)
	if len(st.filtered) == 0 {
		rows = append(rows, "  "+style.paint(clrWarn, fmt.Sprintf("No models match %q", st.query)))
	} else {
		start := st.page() * pickerPageSize
		end := min(start+pickerPageSize, len(st.filtered))
		for i, model := range st.filtered[start:end] {
			selected := start+i == st.cursor
			rows = append(rows, rowLine(
				style.marker(selected),
				style.modelCell(model, selected),
				style.cell(displayModelName(model), wName, clrName),
				style.cell(formatContextLength(model.ContextLength), wCtx, ctxColor(model.ContextLength)),
				style.cell(formatPricePerMillion(model.PromptPrice, model.OutputPrice), wPrice, priceColor(model)),
			))
		}
	}
	for len(rows) < pickerPageSize {
		rows = append(rows, "")
	}
	lines = append(lines, rows...)

	lines = append(lines, "")
	hints := [][2]string{{"↑↓", "browse"}, {"←→", "page"}, {"↵", "select"}}
	if st.hasProviders {
		hints = append(hints, [2]string{"tab", "providers"})
	}
	hints = append(hints, [2]string{"esc", "back"}, [2]string{"^C", "quit"})
	lines = append(lines, footer(style, hints))
	return lines, caretRow, caretCol
}

// Provider view column widths.
const (
	wProvider = 22
	wQuant    = 12
	wPCtx     = 15
)

// renderProviderView builds the provider/endpoint table for the chosen model.
func (a *app) renderProviderView(p *providerState, style terminalStyle) []string {
	lines := make([]string, 0, pickerPageSize+9)
	lines = append(lines, "")

	meta := fmt.Sprintf("%d endpoint", len(p.endpoints))
	if len(p.endpoints) != 1 {
		meta += "s"
	}
	if p.totalPages() > 1 {
		meta += fmt.Sprintf(" · page %d/%d", p.page()+1, p.totalPages())
	}
	lines = append(lines, fmt.Sprintf("%s%s %s   %s",
		style.paint(clrAccent, "▌ "),
		style.header("Providers"),
		style.paint(clrModelHi, p.model.ID),
		style.paint(clrDim, meta),
	))
	lines = append(lines, "", "")

	headerPlain := rowLine(padRight("", wGutter),
		padRight("PROVIDER", wProvider),
		padRight("QUANTIZATION", wQuant),
		padRight("PRICE/M", wPrice),
		padRight("CONTEXT WINDOW", wPCtx),
	)
	lines = append(lines, style.paint(clrDim, headerPlain))
	lines = append(lines, style.paint(clrFaint, strings.Repeat("─", len(strings.TrimRight(headerPlain, " "))+2)))

	rows := make([]string, 0, pickerPageSize)
	switch {
	case p.err != nil:
		rows = append(rows, "  "+style.paint(clrWarn, "Could not load providers: "+p.err.Error()))
	case len(p.endpoints) == 0:
		rows = append(rows, "  "+style.paint(clrWarn, "No providers available for this model."))
	default:
		start := p.page() * pickerPageSize
		end := min(start+pickerPageSize, len(p.endpoints))
		for i, ep := range p.endpoints[start:end] {
			selected := start+i == p.cursor
			nameCode := clrModel
			if selected {
				nameCode = clrAccentBold
			}
			quant := ep.Quantization
			if quant == "" {
				quant = "-"
			}
			rows = append(rows, rowLine(
				style.marker(selected),
				style.cell(ep.ProviderName, wProvider, nameCode),
				style.cell(quant, wQuant, clrName),
				style.cell(formatPricePerMillion(ep.PromptPrice, ep.OutputPrice), wPrice, priceColor(Model{OutputPrice: ep.OutputPrice})),
				style.cell(formatContextLength(ep.ContextLength), wPCtx, ctxColor(ep.ContextLength)),
			))
		}
	}
	for len(rows) < pickerPageSize {
		rows = append(rows, "")
	}
	lines = append(lines, rows...)

	lines = append(lines, "")
	lines = append(lines, footer(style, [][2]string{
		{"↑↓", "browse"}, {"←→", "page"}, {"↵", "launch"}, {"esc", "back"}, {"^C", "quit"},
	}))
	return lines
}

func footer(style terminalStyle, hints [][2]string) string {
	parts := make([]string, 0, len(hints))
	for _, h := range hints {
		parts = append(parts, style.paint(clrAccent, h[0])+" "+style.paint(clrDim, h[1]))
	}
	return "  " + strings.Join(parts, style.paint(clrFaint, "  ·  "))
}
