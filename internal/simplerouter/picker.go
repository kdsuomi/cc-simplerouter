package simplerouter

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"
)

const pickerPageSize = 12

// Palette (24-bit truecolor SGR parameter strings). Warm "clay" accent with a
// cool data palette so prices, context, and tags read at a glance.
const (
	cReset = "\x1b[0m"

	clrAccent     = "38;2;217;119;87"    // clay / coral — the brand accent
	clrAccentBold = "1;38;2;217;119;87"  // accent + bold for the active row
	clrAccent2    = "38;2;234;179;102"   // warm gold, the far end of the wordmark gradient
	clrHead       = "1;38;2;236;236;241" // bright white-ish, bold headings
	clrModel      = "38;2;212;214;224"   // model ids
	clrModelHi    = "1;38;2;240;242;248" // recommended model ids
	clrName       = "38;2;146;150;166"   // vendor display names
	clrDim        = "38;2;108;110;126"   // secondary text
	clrFaint      = "38;2;78;80;96"      // rules / chrome
	clrWarn       = "38;2;233;176;90"    // warnings

	clrCheap  = "38;2;120;201;120" // < $1 / M
	clrModer  = "38;2;226;191;110" // < $5 / M
	clrPricey = "38;2;230;128;110" // >= $5 / M

	clrCtxHuge = "38;2;96;200;214"  // >= 1M context
	clrCtxBig  = "38;2;120;201;140" // >= 100k
	clrCtxSm   = "38;2;120;122;136" // smaller
)

type terminalStyle struct {
	enabled bool
}

func newTerminalStyle(w io.Writer) terminalStyle {
	if os.Getenv("NO_COLOR") != "" {
		return terminalStyle{}
	}
	f, ok := w.(*os.File)
	if !ok {
		return terminalStyle{}
	}
	return terminalStyle{enabled: term.IsTerminal(int(f.Fd()))}
}

// paint wraps text in an SGR color sequence, but only when color is enabled.
// When disabled it returns text unchanged, so non-TTY output stays plain.
func (s terminalStyle) paint(code, text string) string {
	if !s.enabled || text == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + cReset
}

func (s terminalStyle) header(text string) string {
	return s.paint(clrHead, text)
}

func (s terminalStyle) warning(text string) string {
	if text == "" {
		return text
	}
	return s.paint(clrWarn, text)
}

// gradient paints text left-to-right interpolating between two RGB colors.
func (s terminalStyle) gradient(text string, from, to [3]int) string {
	if !s.enabled {
		return text
	}
	runes := []rune(text)
	var b strings.Builder
	for i, r := range runes {
		t := 0.0
		if len(runes) > 1 {
			t = float64(i) / float64(len(runes)-1)
		}
		lerp := func(a, b int) int { return a + int(float64(b-a)*t+0.5) }
		fmt.Fprintf(&b, "\x1b[1;38;2;%d;%d;%dm%c", lerp(from[0], to[0]), lerp(from[1], to[1]), lerp(from[2], to[2]), r)
	}
	b.WriteString(cReset)
	return b.String()
}

// printSetupBanner draws the first-run wordmark. TTY-only: piped/non-color
// output stays clean and the plain status lines carry the needed text.
func printSetupBanner(w io.Writer, style terminalStyle) {
	if !style.enabled {
		return
	}
	from := [3]int{217, 119, 87}
	to := [3]int{234, 179, 102}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %s\n", style.gradient("◇ simplerouter", from, to))
	fmt.Fprintf(w, "  %s\n", style.paint(clrFaint, "claude code · openrouter & gemini models"))
}

// endpointsFunc fetches the provider endpoints for a model id. nil disables the
// provider view (line mode / tests).
type endpointsFunc func(modelID string) ([]Endpoint, error)

// pickResult is the picker's outcome: the chosen model and, optionally, a pinned
// provider endpoint. ProviderTag is the OpenRouter routing slug to inject.
type pickResult struct {
	Model        Model
	ProviderTag  string
	ProviderName string
}

func (a *app) pickModel(title string, models []Model, current string, endpoints endpointsFunc) (pickResult, error) {
	if len(models) == 0 {
		return pickResult{}, errors.New("no models returned")
	}

	models = orderModelsForPicker(models)

	// Use the full-screen arrow-key picker when both ends are real terminals;
	// otherwise fall back to the line-based prompt (also used under tests/pipes).
	if in, out, ok := a.pickerTerminals(); ok {
		res, err := a.pickModelInteractive(in, out, title, models, endpoints)
		if !errors.Is(err, errPickerFallback) {
			return res, err
		}
	}
	model, err := a.pickModelLineMode(title, models, current)
	return pickResult{Model: model}, err
}

func (a *app) pickModelLineMode(title string, models []Model, current string) (Model, error) {
	filter := initialModelFilter(models, current)
	style := newTerminalStyle(a.stderr)
	page := 0

	for {
		filtered := filterModels(models, filter)
		if len(filtered) == 0 {
			fmt.Fprintln(a.stderr, style.paint(clrWarn, fmt.Sprintf("  No models match %q.", filter)))
			filter = ""
			page = 0
			continue
		}

		totalPages := pageCount(len(filtered), pickerPageSize)
		if page >= totalPages {
			page = totalPages - 1
		}
		if page < 0 {
			page = 0
		}

		visible := visibleModels(filtered, page)
		a.printModelPage(title, filtered, visible, filter, page, totalPages, style)
		line, err := a.readLine()
		if err != nil && !errors.Is(err, io.EOF) {
			return Model{}, err
		}
		if line == "" {
			if errors.Is(err, io.EOF) && !isTerminalReader(a.stdin) {
				return Model{}, errors.New("model selection requires interactive input")
			}
			return visible[0], nil
		}

		switch strings.ToLower(line) {
		case "n", "next":
			if page+1 < totalPages {
				page++
			} else {
				fmt.Fprintln(a.stderr, style.paint(clrDim, "  Already on the last page."))
			}
			continue
		case "p", "prev", "previous":
			if page > 0 {
				page--
			} else {
				fmt.Fprintln(a.stderr, style.paint(clrDim, "  Already on the first page."))
			}
			continue
		}

		if strings.HasPrefix(line, "?") {
			if err := a.printModelDetailsCommand(line, visible, style); err != nil {
				fmt.Fprintln(a.stderr, style.paint(clrWarn, "  "+err.Error()))
			}
			continue
		}

		if n, err := strconv.Atoi(line); err == nil {
			if n >= 1 && n <= len(visible) {
				return visible[n-1], nil
			}
			fmt.Fprintln(a.stderr, style.paint(clrWarn, fmt.Sprintf("  Choose a number from 1 to %d.", len(visible))))
			continue
		}

		filter = line
		page = 0
	}
}

func initialModelFilter(models []Model, current string) string {
	current = strings.TrimSpace(current)
	if current == "" {
		return ""
	}
	for _, model := range models {
		if normalizeModelText(model.ID) == normalizeModelText(current) {
			return model.ID
		}
	}
	return current
}

func visibleModels(models []Model, page int) []Model {
	start := page * pickerPageSize
	if start >= len(models) {
		start = max(len(models)-pickerPageSize, 0)
	}
	end := min(start+pickerPageSize, len(models))
	return models[start:end]
}

func pageCount(total, pageSize int) int {
	if total <= 0 {
		return 1
	}
	return (total + pageSize - 1) / pageSize
}

// Column widths for the model table.
const (
	wGutter = 4 // marker + space + 2-digit number
	wModel  = 30
	wName   = 22
	wCtx    = 11
	wPrice  = 14
)

func (a *app) printModelPage(title string, filtered, visible []Model, filter string, page, totalPages int, style terminalStyle) {
	fmt.Fprintln(a.stderr)

	// Title bar: accent ruler glyph, bold title, dim meta.
	count := fmt.Sprintf("%d match", len(filtered))
	if len(filtered) != 1 {
		count += "es"
	}
	meta := fmt.Sprintf("%s · page %d/%d", count, page+1, totalPages)
	fmt.Fprintf(a.stderr, "%s%s   %s\n",
		style.paint(clrAccent, "▌ "),
		style.header(title),
		style.paint(clrDim, meta),
	)
	if filter != "" {
		fmt.Fprintf(a.stderr, "  %s %s\n",
			style.paint(clrDim, "search"),
			style.paint(clrAccent, filter),
		)
	}
	fmt.Fprintln(a.stderr)

	// Header + rule, sized to the exact plain width of a row.
	headerPlain := rowLine(padRight("", wGutter),
		padRight("MODEL", wModel),
		padRight("NAME", wName),
		padRight("CTX", wCtx),
		padRight("PRICE/M", wPrice),
	)
	fmt.Fprintln(a.stderr, style.paint(clrDim, headerPlain))
	fmt.Fprintln(a.stderr, style.paint(clrFaint, strings.Repeat("─", len(strings.TrimRight(headerPlain, " "))+2)))

	for i, model := range visible {
		selected := i == 0
		fmt.Fprintln(a.stderr, rowLine(
			style.gutter(i+1, selected),
			style.modelCell(model, selected),
			style.cell(displayModelName(model), wName, clrName),
			style.cell(formatContextLength(model.ContextLength), wCtx, ctxColor(model.ContextLength)),
			style.cell(formatPricePerMillion(model.PromptPrice, model.OutputPrice), wPrice, priceColor(model)),
		))
	}

	fmt.Fprintln(a.stderr)
	a.printHint(style)
	fmt.Fprint(a.stderr, style.paint(clrAccentBold, "  ❯ "))
}

func rowLine(gutter string, cells ...string) string {
	return gutter + " " + strings.Join(cells, " ")
}

// gutter renders the row marker and number, e.g. "▸  1" for the active row.
func (s terminalStyle) gutter(n int, selected bool) string {
	marker := " "
	if selected {
		marker = ">"
		if s.enabled {
			marker = "▸"
		}
	}
	text := fmt.Sprintf("%s %2d", marker, n)
	if selected {
		return s.paint(clrAccentBold, text)
	}
	return s.paint(clrDim, text)
}

func (s terminalStyle) modelCell(model Model, selected bool) string {
	code := clrModel
	switch {
	case selected:
		code = clrAccentBold
	case isRecommendedModel(model.ID):
		code = clrModelHi
	}
	return s.cell(model.ID, wModel, code)
}

// cell renders a single fixed-width column: truncate, pad, then colorize.
func (s terminalStyle) cell(text string, width int, code string) string {
	return s.paint(code, padRight(fitText(text, width), width))
}

func padRight(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

func ctxColor(n int) string {
	switch {
	case n >= 1_000_000:
		return clrCtxHuge
	case n >= 100_000:
		return clrCtxBig
	default:
		return clrCtxSm
	}
}

func priceColor(m Model) string {
	perM := parsePricePerMillion(m.OutputPrice)
	switch {
	case perM < 0:
		return clrDim
	case perM < 1:
		return clrCheap
	case perM < 5:
		return clrModer
	default:
		return clrPricey
	}
}

func parsePricePerMillion(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return -1
	}
	return v * 1_000_000
}

// printHint draws the footer legend of keyboard actions.
func (a *app) printHint(style terminalStyle) {
	hints := []struct{ key, desc string }{
		{"↵", "select"},
		{"1-9", "pick"},
		{"type", "search"},
		{"n/p", "page"},
		{"? N", "details"},
	}
	parts := make([]string, 0, len(hints))
	for _, h := range hints {
		parts = append(parts, style.paint(clrAccent, h.key)+" "+style.paint(clrDim, h.desc))
	}
	fmt.Fprintln(a.stderr, "  "+strings.Join(parts, style.paint(clrFaint, "  ·  ")))
}

func (a *app) printModelDetailsCommand(line string, visible []Model, style terminalStyle) error {
	raw := strings.TrimSpace(strings.TrimPrefix(line, "?"))
	if raw == "" {
		return errors.New("Type ? N to show details for a visible model.")
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > len(visible) {
		return fmt.Errorf("Choose a detail number from 1 to %d.", len(visible))
	}
	a.printModelDetails(visible[n-1], style)
	return nil
}

func (a *app) printModelDetails(model Model, style terminalStyle) {
	fmt.Fprintln(a.stderr)
	fmt.Fprintf(a.stderr, "%s%s\n", style.paint(clrAccent, "▌ "), style.header("Model details"))
	field := func(label, value, code string) {
		fmt.Fprintf(a.stderr, "  %s %s\n", style.paint(clrDim, padRight(label, 9)), style.paint(code, value))
	}
	field("ID", model.ID, clrModelHi)
	field("Name", displayModelName(model), clrName)
	field("Context", formatContextLength(model.ContextLength)+" tokens", ctxColor(model.ContextLength))
	field("Price/M", formatPricePerMillion(model.PromptPrice, model.OutputPrice), priceColor(model))
	field("Tags", strings.Join(modelTags(model), ", "), clrModel)
	if warning := modelWarning(model); warning != "" {
		fmt.Fprintf(a.stderr, "  %s %s\n", style.paint(clrDim, padRight("Warning", 9)), style.warning(warning))
	}
	if len(model.SupportedParameters) > 0 {
		fmt.Fprintf(a.stderr, "  %s %s\n", style.paint(clrDim, padRight("Params", 9)), style.paint(clrName, strings.Join(model.SupportedParameters, ", ")))
	}
}

func displayModelName(model Model) string {
	if strings.TrimSpace(model.Name) == "" {
		return "-"
	}
	return model.Name
}

func fitText(text string, width int) string {
	if width <= 0 {
		return ""
	}
	if len(text) > width {
		if width == 1 {
			return "~"
		}
		text = text[:width-1] + "~"
	}
	return text
}

func isTerminalReader(r io.Reader) bool {
	f, ok := r.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}
