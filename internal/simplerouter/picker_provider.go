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

// pickOption is one row in a small fixed-choice picker (e.g. the provider
// selection shown before the model picker).
type pickOption struct {
	ID     string // stable id, e.g. providerOpenRouter
	Label  string // "OpenRouter"
	Detail string // dim right-hand description
}

func providerOptions() []pickOption {
	return []pickOption{
		{ID: providerOpenRouter, Label: "OpenRouter", Detail: "400+ models, one API key"},
		{ID: providerGemini, Label: "Google AI Studio", Detail: "Gemini models, direct from Google"},
		{ID: providerOpenAI, Label: "OpenAI", Detail: "GPT models, direct from OpenAI"},
		{ID: providerDeepSeek, Label: "DeepSeek", Detail: "DeepSeek models, Anthropic API"},
		{ID: providerZAI, Label: "Z.AI", Detail: "GLM models, direct from Z.AI"},
	}
}

// pickOne shows a small single-choice picker, preselecting currentID. Mirrors
// pickModel's dispatch: interactive when both ends are real terminals, line
// mode otherwise.
func (a *app) pickOne(title string, opts []pickOption, currentID string) (pickOption, error) {
	if len(opts) == 0 {
		return pickOption{}, errors.New("no options to pick from")
	}
	cursor := 0
	for i, opt := range opts {
		if opt.ID == currentID {
			cursor = i
			break
		}
	}
	if in, out, ok := a.pickerTerminals(); ok {
		opt, err := a.pickOneInteractive(in, out, title, opts, cursor)
		if !errors.Is(err, errPickerFallback) {
			return opt, err
		}
	}
	return a.pickOneLineMode(title, opts, cursor)
}

// Option picker column widths.
const (
	wOptLabel  = 20
	wOptDetail = 36
)

// optionState holds the cursor for the fixed-choice picker; pure so its
// navigation can be unit tested without a terminal.
type optionState struct {
	opts   []pickOption
	cursor int
}

func (o *optionState) clamp() {
	if o.cursor > len(o.opts)-1 {
		o.cursor = len(o.opts) - 1
	}
	if o.cursor < 0 {
		o.cursor = 0
	}
}

func (o *optionState) handleInput(buf []byte) pickerAction {
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
					o.cursor--
				case 'B':
					o.cursor++
				case 'H':
					o.cursor = 0
				case 'F':
					o.cursor = len(o.opts) - 1
				}
				o.clamp()
				i = j
			} else if i+1 >= len(buf) {
				return pickerQuit
			}
		case b == '\r' || b == '\n':
			return pickerSelect
		case b == 0x03: // Ctrl+C
			return pickerQuit
		}
	}
	return pickerNone
}

func (a *app) pickOneInteractive(in, out *os.File, title string, opts []pickOption, cursor int) (pickOption, error) {
	if _, h, err := term.GetSize(int(out.Fd())); err == nil && h > 0 && h < len(opts)+7 {
		return pickOption{}, errPickerFallback
	}
	fd := int(in.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return pickOption{}, errPickerFallback
	}
	defer term.Restore(fd, oldState)
	defer io.WriteString(out, "\x1b[0 q\x1b[?25h")
	enableTerminalVT(in.Fd(), out.Fd())

	style := newTerminalStyle(out)
	st := &optionState{opts: opts, cursor: cursor}
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
		lines := renderOptionView(title, st, style, false)
		lastN = len(lines)
		drawBlockCursor(out, lines, &prevRow, -1, 0)

		n, rerr := in.Read(buf)
		if n > 0 {
			switch st.handleInput(buf[:n]) {
			case pickerSelect:
				sel := st.opts[st.cursor]
				finish("  " + style.paint(clrAccentBold, "▸ selected ") + style.paint(clrModelHi, sel.Label))
				return sel, nil
			case pickerQuit:
				finish("")
				return pickOption{}, errPickerCancelled
			}
		}
		if rerr != nil {
			finish("")
			return pickOption{}, errPickerCancelled
		}
	}
}

// renderOptionView builds the option table block. Constant height so the
// interactive redraw is stable. numbered adds the line-mode selection digits.
func renderOptionView(title string, st *optionState, style terminalStyle, numbered bool) []string {
	lines := make([]string, 0, len(st.opts)+7)
	lines = append(lines, "")

	meta := fmt.Sprintf("%d options", len(st.opts))
	lines = append(lines, fmt.Sprintf("%s%s   %s",
		style.paint(clrAccent, "▌ "),
		style.header(title),
		style.paint(clrDim, meta),
	))
	lines = append(lines, "")

	headerPlain := rowLine(padRight("", wGutter),
		padRight("PROVIDER", wOptLabel),
		padRight("MODELS", wOptDetail),
	)
	lines = append(lines, style.paint(clrDim, headerPlain))
	lines = append(lines, style.paint(clrFaint, strings.Repeat("─", len(strings.TrimRight(headerPlain, " "))+2)))

	for i, opt := range st.opts {
		selected := i == st.cursor
		labelCode := clrModel
		if selected {
			labelCode = clrAccentBold
		}
		gutter := style.marker(selected)
		if numbered {
			gutter = style.gutter(i+1, selected)
		}
		lines = append(lines, rowLine(
			gutter,
			style.cell(opt.Label, wOptLabel, labelCode),
			style.cell(opt.Detail, wOptDetail, clrName),
		))
	}

	lines = append(lines, "")
	if numbered {
		lines = append(lines, footer(style, [][2]string{
			{"↵", "select"}, {fmt.Sprintf("1-%d", len(st.opts)), "pick"},
		}))
	} else {
		lines = append(lines, footer(style, [][2]string{
			{"↑↓", "browse"}, {"↵", "select"}, {"esc", "quit"},
		}))
	}
	return lines
}

func (a *app) pickOneLineMode(title string, opts []pickOption, cursor int) (pickOption, error) {
	style := newTerminalStyle(a.stderr)
	st := &optionState{opts: opts, cursor: cursor}
	for {
		for _, line := range renderOptionView(title, st, style, true) {
			fmt.Fprintln(a.stderr, line)
		}
		fmt.Fprint(a.stderr, style.paint(clrAccentBold, "  ❯ "))
		line, err := a.readLine()
		if err != nil && !errors.Is(err, io.EOF) {
			return pickOption{}, err
		}
		if line == "" {
			// Plain Enter (or scripted EOF) keeps the preselected option.
			return opts[st.cursor], nil
		}
		if n, aerr := strconv.Atoi(line); aerr == nil && n >= 1 && n <= len(opts) {
			return opts[n-1], nil
		}
		fmt.Fprintln(a.stderr, style.paint(clrWarn, fmt.Sprintf("  Choose a number from 1 to %d.", len(opts))))
		if errors.Is(err, io.EOF) {
			return pickOption{}, errors.New("selection requires interactive input")
		}
	}
}
