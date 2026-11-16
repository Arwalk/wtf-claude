package render

import "strings"

// style is a bit set of text attributes.
type style uint16

const (
	plain style = 0
	bold  style = 1 << iota
	dim
	red
	green
	yellow
	cyan
	magenta
)

// sgr returns the ANSI SGR parameters for st. "Dim" uses bright black (90)
// rather than SGR 2 because it is supported everywhere, including by
// tview's ANSI translation inside wtfutil.
func (st style) sgr() string {
	var p []string
	if st&bold != 0 {
		p = append(p, "1")
	}
	switch {
	case st&red != 0:
		p = append(p, "31")
	case st&green != 0:
		p = append(p, "32")
	case st&yellow != 0:
		p = append(p, "33")
	case st&cyan != 0:
		p = append(p, "36")
	case st&magenta != 0:
		p = append(p, "35")
	case st&dim != 0:
		p = append(p, "90")
	}
	return strings.Join(p, ";")
}

type seg struct {
	text string
	st   style
}

// line is a sequence of styled segments.
type line []seg

func (l line) width() int {
	n := 0
	for _, s := range l {
		n += cellWidth(s.text)
	}
	return n
}

func (l line) plainText() string {
	var b strings.Builder
	for _, s := range l {
		b.WriteString(s.text)
	}
	return b.String()
}

// truncate cuts the line to at most w cells, ending with "…" if cut.
func (l line) truncate(w int) line {
	if w <= 0 {
		return nil
	}
	if l.width() <= w {
		return l
	}
	var out line
	used := 0
	for _, s := range l {
		sw := cellWidth(s.text)
		if used+sw <= w-1 {
			out = append(out, s)
			used += sw
			continue
		}
		out = append(out, seg{truncCells(s.text, w-1-used), s.st}, seg{"…", s.st})
		return out
	}
	return out
}

// render writes the line using ANSI colours when color is true.
func (l line) render(color bool) string {
	var b strings.Builder
	for _, s := range l {
		text := stripControls(s.text)
		if text == "" {
			continue
		}
		if color && s.st != plain {
			b.WriteString("\x1b[")
			b.WriteString(s.st.sgr())
			b.WriteString("m")
			b.WriteString(text)
			b.WriteString("\x1b[0m")
		} else {
			b.WriteString(text)
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// stripControls removes characters a terminal would act on instead of
// printing: C0/C1 control codes (ESC starts escape sequences, a newline would
// add a row) and Unicode bidirectional overrides, which can reorder what is
// displayed. Labels come from an undocumented API and from transcript files,
// so every segment passes through here before it is written.
func stripControls(s string) string {
	if strings.IndexFunc(s, unsafeRune) < 0 {
		return s
	}
	return strings.Map(func(r rune) rune {
		if unsafeRune(r) {
			return -1
		}
		return r
	}, s)
}

// unsafeRune reports whether r is a control or bidi-override character.
func unsafeRune(r rune) bool {
	return r < 0x20 || (r >= 0x7f && r < 0xa0) ||
		(r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
}

// cellWidth approximates the terminal column width of s.
func cellWidth(s string) int {
	n := 0
	for _, r := range s {
		n += runeWidth(r)
	}
	return n
}

func runeWidth(r rune) int {
	switch {
	case r == 0, r < 32, r >= 0x7f && r < 0xa0:
		return 0
	case r >= 0x300 && r <= 0x36f, r == 0x200b, r == 0x200d, r >= 0xfe00 && r <= 0xfe0f:
		return 0 // combining marks / zero-width
	case r >= 0x1100 && r <= 0x115f, r >= 0x2e80 && r <= 0xa4cf, r >= 0xac00 && r <= 0xd7a3,
		r >= 0xf900 && r <= 0xfaff, r >= 0xfe30 && r <= 0xfe4f, r >= 0xff00 && r <= 0xff60,
		r >= 0xffe0 && r <= 0xffe6, r >= 0x1f300 && r <= 0x1faff, r >= 0x20000 && r <= 0x3fffd:
		return 2
	}
	return 1
}

// truncCells returns the longest prefix of s that fits in w cells.
func truncCells(s string, w int) string {
	if w <= 0 {
		return ""
	}
	n := 0
	for i, r := range s {
		rw := runeWidth(r)
		if n+rw > w {
			return s[:i]
		}
		n += rw
	}
	return s
}

// fitText truncates s to w cells with an ellipsis.
func fitText(s string, w int) string {
	if cellWidth(s) <= w {
		return s
	}
	if w <= 1 {
		return truncCells("…", w)
	}
	return truncCells(s, w-1) + "…"
}

// padRight pads s with spaces to w cells (no truncation).
func padRight(s string, w int) string {
	if d := w - cellWidth(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// padLeft right-aligns s in w cells.
func padLeft(s string, w int) string {
	if d := w - cellWidth(s); d > 0 {
		return strings.Repeat(" ", d) + s
	}
	return s
}
