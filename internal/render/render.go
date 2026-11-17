// Package render formats a report.Report as ANSI/plain text or JSON.
package render

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"claude-usage/internal/localstats"
	"claude-usage/internal/report"
)

// MinWidth is the narrowest layout supported; narrower requests are clamped.
const MinWidth = 12

// Options controls text rendering.
type Options struct {
	Width int
	// Height is the number of rows available (0 = unlimited). When the
	// output would be taller, less important lines are dropped first (see
	// Lines).
	Height  int
	Color   bool
	Compact bool
	// Loc is used for wall-clock times and "today" (defaults to time.Local).
	Loc *time.Location
}

// JSON writes the report as indented JSON.
func JSON(w io.Writer, r *report.Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(r)
}

// Text writes the report as text, with ANSI colours if opt.Color.
func Text(w io.Writer, r *report.Report, opt Options) error {
	lines := Lines(r, opt)
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// Detail-reduction levels of the full layout, applied cumulatively when the
// output is taller than Options.Height.
const (
	trimShortLabels    = 1 + iota // limits side by side with short labels
	trimModels                    // drop the Models line
	trimDetailsOneLine            // at most one reset/pace line per limit
	trimBreakdown                 // drop the surface breakdown
	trimLocalHeader               // drop the "Local Claude Code" header
	trimDetails                   // drop reset/pace lines
	trimLocal                     // drop local stats
	trimPlanSpend                 // drop plan and spend
	maxTrim            = trimPlanSpend
)

// Lines renders the report into output lines.
//
// With a Height, the full layout sheds detail until it fits, in this order:
// stacked limit rows (short labels side by side instead), the Models line,
// extra detail lines (one reset/pace line per limit), the surface breakdown,
// the local-stats header, all reset/pace lines, the local stats, then plan
// and spend. If even that is too tall it falls back to the
// compact layout, which keeps the most important limits (the binding one
// first, then the most used) and says how many were left out.
func Lines(r *report.Report, opt Options) []string {
	if opt.Loc == nil {
		opt.Loc = time.Local
	}
	if opt.Width < MinWidth {
		opt.Width = MinWidth
	}
	var lines []line
	if !opt.Compact {
		for trim := 0; trim <= maxTrim; trim++ {
			rd := &renderer{opt: opt, now: r.GeneratedAt, w: opt.Width, trim: trim, stale: r.Status.Stale}
			rd.full(r)
			lines = rd.lines
			if opt.Height <= 0 || len(lines) <= opt.Height {
				break
			}
			lines = nil
		}
	}
	if lines == nil {
		rd := &renderer{opt: opt, now: r.GeneratedAt, w: opt.Width, stale: r.Status.Stale}
		rd.compact(r)
		lines = rd.lines
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, l.truncate(opt.Width).render(opt.Color))
	}
	return out
}

type renderer struct {
	opt   Options
	now   time.Time
	w     int
	lines []line
	// trim is the detail-reduction level of the full layout (0 = all).
	trim int
	// stale is true when a refresh failed, so nothing is being refreshed.
	stale bool
}

func (rd *renderer) add(l line) { rd.lines = append(rd.lines, l) }

func sevStyle(sev string) style {
	switch sev {
	case report.SeverityCritical:
		return red
	case report.SeverityWarning:
		return yellow
	case report.SeverityUnknown:
		return dim
	}
	return green
}

/* ----------------------------- full layout ----------------------------- */

func (rd *renderer) full(r *report.Report) {
	auth := authNote(r.Status)
	if auth {
		rd.status(r)
	}
	if r.Plan != nil && r.Plan.Display != "" && rd.trim < trimPlanSpend {
		rd.add(line{{fitText(r.Plan.Display, rd.w), bold}})
	}
	rd.limits(r.Limits, false)
	if !auth {
		rd.status(r)
	}
	if r.Breakdown != nil && rd.trim < trimBreakdown {
		rd.breakdown(r.Breakdown)
	}
	if r.Spend != nil && rd.trim < trimPlanSpend {
		rd.spend(r.Spend)
	}
	if r.Local != nil && rd.trim < trimLocal {
		rd.local(r.Local, r.Status.LocalError)
	}
	if len(rd.lines) == 0 {
		rd.add(line{{"nothing to show", dim}})
	}
}

func (rd *renderer) compact(r *report.Report) {
	ls := r.Limits
	rd.status(r)
	note := rd.lines
	rd.lines = nil
	// An auth note comes first and is kept even with room for one line:
	// the limits under it may be days old.
	auth := authNote(r.Status)
	hidden := 0
	if h := rd.opt.Height; h > 0 {
		budget := h
		if len(note) > 0 && (h >= 2 || auth) {
			budget--
		} else {
			note = nil
		}
		if len(ls) > budget {
			keep := budget
			if keep >= 3 {
				keep-- // room for "+N more"
			}
			hidden = len(ls) - keep
			ls = mostImportant(ls, keep)
		}
	}
	if auth {
		rd.lines = append(rd.lines, note...)
		note = nil
	}
	rd.limits(ls, true)
	if hidden > 0 && len(rd.lines)+len(note) < rd.opt.Height {
		rd.add(line{{fitText(fmt.Sprintf("+%d more", hidden), rd.w), dim}})
	}
	rd.lines = append(rd.lines, note...)
	if len(rd.lines) == 0 && r.Local != nil {
		rd.local(r.Local, r.Status.LocalError)
	}
	if len(rd.lines) == 0 {
		rd.add(line{{"no usage data", dim}})
	}
	if h := rd.opt.Height; h > 0 && len(rd.lines) > h {
		rd.lines = rd.lines[:h]
	}
}

// mostImportant returns the n most important limits in their original
// order: the binding (active) limit first, then by percent used; limits
// with unknown usage come last.
func mostImportant(ls []report.Limit, n int) []report.Limit {
	if n <= 0 {
		return nil
	}
	idx := make([]int, len(ls))
	for i := range idx {
		idx[i] = i
	}
	score := func(l report.Limit) float64 {
		if l.Active {
			return 1e9
		}
		if u, ok := l.Used(); ok {
			return u
		}
		return -1
	}
	sort.SliceStable(idx, func(a, b int) bool { return score(ls[idx[a]]) > score(ls[idx[b]]) })
	idx = idx[:n]
	sort.Ints(idx)
	out := make([]report.Limit, 0, n)
	for _, i := range idx {
		out = append(out, ls[i])
	}
	return out
}

// leftText is the "% left" column for a limit. It is rounded down (99.6%
// used is "<1% left", never "0% left" until the limit is reached), matching
// the thresholds behind its colour.
func leftText(l report.Limit) string {
	if l.Expired {
		return "reset"
	}
	used, ok := l.Used()
	switch {
	case !ok:
		return "? left"
	case used >= 100:
		return "0% left"
	}
	left := report.DisplayLeft(used)
	if left < 1 {
		return "<1% left"
	}
	return fmt.Sprintf("%.0f%% left", left)
}

// bar draws the share used; unknown usage is an empty, dim bar.
func (rd *renderer) bar(l report.Limit, width int) line {
	if width <= 0 {
		return nil
	}
	pct, ok := l.Used()
	if !ok {
		return line{{strings.Repeat("░", width), dim}}
	}
	filled := int(math.Floor(math.Min(math.Max(pct, 0), 100)/100*float64(width) + 0.5))
	return line{
		{strings.Repeat("█", filled), sevStyle(l.Severity)},
		{strings.Repeat("░", width-filled), dim},
	}
}

// limitLayout is one way of laying out the side-by-side limit rows.
type limitLayout struct {
	labels         []string
	labelW, resetW int
	bw             int
}

func (ll limitLayout) truncates() bool {
	for _, s := range ll.labels {
		if cellWidth(s) > ll.labelW {
			return true
		}
	}
	return false
}

func (rd *renderer) layout(labels []string, markerW, leftW, resetW int) limitLayout {
	labelW := 0
	for _, s := range labels {
		labelW = max(labelW, cellWidth(s))
	}
	// Very long (unknown) labels are capped so the bar keeps some room.
	labelW = min(labelW, max(12, rd.w/2))
	extra := 0
	if resetW > 0 {
		extra = resetW + 1
	}
	return limitLayout{labels: labels, labelW: labelW, resetW: resetW,
		bw: rd.w - markerW - labelW - 1 - 1 - leftW - extra}
}

// distinct reports whether all labels differ.
func distinct(labels []string) bool {
	seen := map[string]bool{}
	for _, s := range labels {
		if seen[s] {
			return false
		}
		seen[s] = true
	}
	return true
}

func maxCells(ss []string) int {
	n := 0
	for _, s := range ss {
		n = max(n, cellWidth(s))
	}
	return n
}

func (rd *renderer) limits(ls []report.Limit, compact bool) {
	if len(ls) == 0 {
		return
	}
	markerW := 0
	for _, l := range ls {
		if l.Active {
			markerW = 1
		}
	}
	full := make([]string, len(ls))
	short := make([]string, len(ls))
	leftW, resetW := 0, 0
	for i, l := range ls {
		full[i] = l.Label
		short[i] = l.ShortLabel
		if short[i] == "" {
			short[i] = l.Label
		}
		leftW = max(leftW, cellWidth(leftText(l)))
		if compact {
			resetW = max(resetW, cellWidth(rd.compactReset(l)))
		}
	}
	// Short labels ("All", "Fable", "5h") are only usable when they still
	// tell the limits apart.
	haveShort := distinct(short)
	w := rd.w
	const minBar = 5

	// Candidate layouts in order of preference. Short labels beat cutting
	// labels down to an identical prefix; in compact mode keeping the reset
	// countdown beats keeping the long labels.
	var cands []limitLayout
	add := func(labels []string, rw int) {
		if labels != nil {
			cands = append(cands, rd.layout(labels, markerW, leftW, rw))
		}
	}
	var shortOrNil []string
	if haveShort {
		shortOrNil = short
	}
	if compact && resetW > 0 {
		add(full, resetW)
		add(shortOrNil, resetW)
	}
	add(full, 0)
	add(shortOrNil, 0)
	// With rows to spare the full layout keeps the descriptive labels,
	// stacking each bar under its label when they don't fit side by side.
	if !compact && rd.trim < trimShortLabels {
		cands = cands[:1]
	}
	var chosen *limitLayout
	for i := range cands {
		if cands[i].bw >= minBar && !cands[i].truncates() {
			chosen = &cands[i]
			break
		}
	}
	stackFull := !compact && rd.trim < trimShortLabels && maxCells(full) <= w-markerW
	if chosen == nil && !stackFull {
		// Some label must be cut: cut the short ones if possible.
		for _, labels := range [][]string{shortOrNil, full} {
			if labels == nil {
				continue
			}
			if ll := rd.layout(labels, markerW, leftW, 0); ll.bw >= minBar {
				chosen = &ll
				break
			}
		}
	}
	// pick returns the full label if every full label fits in avail cells,
	// otherwise the short one.
	pick := func(i, avail int) string {
		if haveShort && maxCells(full) > avail {
			return short[i]
		}
		return full[i]
	}

	for i, l := range ls {
		marker := seg{"", plain}
		if markerW > 0 {
			marker = seg{" ", plain}
			if l.Active {
				marker = seg{"▸", cyan | bold}
			}
		}
		labelSt := plain
		if l.Active {
			labelSt = bold
		}
		lt := leftText(l)
		leftSt := sevStyle(l.Severity) | bold
		if _, ok := l.Used(); !ok {
			leftSt = dim
		}
		left := seg{padLeft(lt, leftW), leftSt}
		switch {
		case chosen != nil:
			row := line{marker, {padRight(fitText(chosen.labels[i], chosen.labelW), chosen.labelW), labelSt}, {" ", plain}}
			row = append(row, rd.bar(l, chosen.bw)...)
			row = append(row, seg{" ", plain}, left)
			if chosen.resetW > 0 {
				row = append(row, seg{" ", plain}, seg{padLeft(rd.compactReset(l), chosen.resetW), dim})
			}
			rd.add(row)
		case compact:
			// label … NN% left (no room for a bar). When even the short
			// labels don't fit, drop the word "left" before cutting them.
			lw := max(1, w-markerW-1-leftW)
			lft := leftW
			if maxCells(short) > lw {
				lft = 0
				for _, x := range ls {
					lft = max(lft, cellWidth(strings.TrimSuffix(leftText(x), " left")))
				}
				left.text = strings.TrimSuffix(lt, " left")
				lw = max(1, w-markerW-1-lft)
			}
			lab := fitText(pick(i, lw), lw)
			gap := max(1, w-markerW-cellWidth(lab)-cellWidth(left.text))
			rd.add(line{marker, {lab, labelSt}, {strings.Repeat(" ", gap), plain}, left})
		default:
			// Row 1: the label. Row 2: bar + "NN% left", the bar the same
			// length for every limit.
			rd.add(line{marker, {fitText(pick(i, w-markerW), w-markerW), labelSt}})
			row := line{{strings.Repeat(" ", markerW), plain}}
			if b := w - markerW - 1 - leftW; b >= 3 {
				row = append(row, rd.bar(l, b)...)
				row = append(row, seg{" ", plain})
			} else {
				left.text = lt
			}
			rd.add(append(row, left))
		}
		if !compact && rd.trim < trimDetails {
			rd.details(l, markerW)
		}
	}
}

// compactReset is the short reset countdown used in compact mode.
func (rd *renderer) compactReset(l report.Limit) string {
	if l.ResetsAt == nil || l.Expired {
		return ""
	}
	return "↻" + CompactDuration(l.ResetsAt.Sub(rd.now))
}

// part is a piece of a detail line with progressively shorter variants.
type part struct {
	variants []string
	st       style
}

func (p part) at(level int) string {
	if level >= len(p.variants) {
		return p.variants[len(p.variants)-1]
	}
	return p.variants[level]
}

// bestFit returns the longest variant of p that fits in avail cells.
func (p part) bestFit(avail int) string {
	for _, v := range p.variants {
		if cellWidth(v) <= avail {
			return v
		}
	}
	return p.variants[len(p.variants)-1]
}

// detailParts builds the reset / pace / run-out pieces for a limit.
func (rd *renderer) detailParts(l report.Limit) (reset *part, pace []part) {
	if l.Expired {
		if rd.stale {
			// The row already says "reset", and nothing is being refreshed.
			return nil, nil
		}
		return &part{[]string{"reset · refreshing", "reset"}, dim}, nil
	}
	if l.ResetsAt != nil {
		d := l.ResetsAt.Sub(rd.now)
		if d <= 0 {
			reset = &part{[]string{"resetting now"}, dim}
		} else {
			clock := WallClock(*l.ResetsAt, rd.now, rd.opt.Loc)
			reset = &part{[]string{
				fmt.Sprintf("resets in %s (%s)", Duration(d), clock),
				fmt.Sprintf("resets in %s", Duration(d)),
				fmt.Sprintf("↻ %s", CompactDuration(d)),
			}, dim}
		}
	}
	p := l.Pace
	if p == nil {
		return reset, nil
	}
	delta := math.Abs(p.DeltaPercent)
	switch {
	case l.PercentUsed != nil && *l.PercentUsed >= 100:
		return reset, []part{{[]string{"limit reached"}, red | bold}}
	case p.Status == report.PaceOver:
		pace = append(pace, part{[]string{
			fmt.Sprintf("%s over pace", Percent(delta)),
			fmt.Sprintf("+%s pace", Percent(delta)),
			fmt.Sprintf("+%s", Percent(delta)),
		}, yellow})
	case p.Status == report.PaceUnder:
		pace = append(pace, part{[]string{
			fmt.Sprintf("%s under pace", Percent(delta)),
			fmt.Sprintf("-%s pace", Percent(delta)),
			fmt.Sprintf("-%s", Percent(delta)),
		}, green})
	default:
		pace = append(pace, part{[]string{"on pace"}, green})
	}
	if p.RunsOutAt != nil {
		d := p.RunsOutAt.Sub(rd.now)
		clock := WallClock(*p.RunsOutAt, rd.now, rd.opt.Loc)
		pace = append(pace, part{[]string{
			fmt.Sprintf("runs out in %s (%s)", Duration(d), clock),
			fmt.Sprintf("out in %s (%s)", Duration(d), clock),
			fmt.Sprintf("out in %s", Duration(d)),
			fmt.Sprintf("out %s", CompactDuration(d)),
		}, red})
	}
	return reset, pace
}

// joinParts renders parts at the given variant levels, separated by " · ".
func joinParts(indent string, parts []part, levels []int) line {
	l := line{{indent, plain}}
	for i, p := range parts {
		if i > 0 {
			l = append(l, seg{" · ", dim})
		}
		l = append(l, seg{p.at(levels[i]), p.st})
	}
	return l
}

// fitParts tries to fit parts on one line, degrading wording in the order
// given by steps ({part index, variant level}). It returns nil if nothing
// fits.
func fitParts(indent string, parts []part, avail int, steps [][2]int) line {
	levels := make([]int, len(parts))
	try := func() line {
		l := joinParts(indent, parts, levels)
		if l.width()-cellWidth(indent) <= avail {
			return l
		}
		return nil
	}
	if l := try(); l != nil {
		return l
	}
	for _, s := range steps {
		if s[0] < 0 || s[0] >= len(parts) || s[1] >= len(parts[s[0]].variants) {
			continue
		}
		levels[s[0]] = s[1]
		if l := try(); l != nil {
			return l
		}
	}
	return nil
}

// details renders the reset countdown and pace for a limit: on one line
// when it fits with light abbreviation, otherwise reset on one line and
// pace on the next, otherwise one piece per line.
func (rd *renderer) details(l report.Limit, markerW int) {
	indent := strings.Repeat(" ", markerW+1)
	if rd.w < 24 {
		indent = strings.Repeat(" ", markerW)
	}
	avail := rd.w - cellWidth(indent)
	reset, pace := rd.detailParts(l)
	var all []part
	if reset != nil {
		all = append(all, *reset)
	}
	all = append(all, pace...)
	if len(all) == 0 {
		return
	}
	// Indices into all.
	ro := -1
	i := 0
	if reset != nil {
		i = 1
	}
	if len(pace) > 1 {
		ro = i + 1
	}
	// 1. Single line, shortening only the run-out wording.
	if ln := fitParts(indent, all, avail, [][2]int{{ro, 1}, {ro, 2}}); ln != nil {
		rd.add(ln)
		return
	}
	if rd.trim >= trimDetailsOneLine {
		// Short on rows: one line per limit, abbreviating everything,
		// else just the first piece (the reset countdown).
		if ln := fitParts(indent, all, avail, [][2]int{{ro, 1}, {ro, 2}, {ro, 3}, {i, 1}, {0, 1}, {i, 2}, {0, 2}}); ln != nil {
			rd.add(ln)
			return
		}
		rd.add(line{{indent, plain}, {all[0].bestFit(avail), all[0].st}})
		return
	}
	// 2. Reset on its own line, pace (+ run-out) on the next.
	if reset != nil && len(pace) > 0 {
		paceLine := fitParts(indent, pace, avail, [][2]int{{1, 1}, {1, 2}, {0, 1}, {1, 3}, {0, 2}})
		if paceLine != nil && cellWidth(reset.at(len(reset.variants)-1)) <= avail {
			rd.add(line{{indent, plain}, {reset.bestFit(avail), reset.st}})
			rd.add(paceLine)
			return
		}
	}
	// 3. One piece per line.
	for _, p := range all {
		rd.add(line{{indent, plain}, {p.bestFit(avail), p.st}})
	}
}

// authNote reports whether the status note is an auth problem, which is
// shown first and in red instead of as a dim stale note.
func authNote(s report.Status) bool { return s.AuthError && s.UsageError != "" }

func (rd *renderer) status(r *report.Report) {
	s := r.Status
	switch {
	case authNote(s):
		// The action leads, so it survives truncation in narrow widgets.
		text := s.UsageError
		if s.Stale && s.UsageFetchedAt != nil {
			text += " · data " + Duration(rd.now.Sub(*s.UsageFetchedAt)) + " old"
		}
		rd.add(line{{fitText(text, rd.w), red}})
	case s.Stale && s.UsageFetchedAt != nil:
		ago := Duration(rd.now.Sub(*s.UsageFetchedAt))
		text := fmt.Sprintf("stale · fetched %s ago", ago)
		if s.UsageError != "" {
			text += " · " + s.UsageError
		}
		rd.add(line{{fitText(text, rd.w), dim}})
	case s.UsageError != "" && len(r.Limits) == 0:
		rd.add(line{{fitText("usage unavailable: "+s.UsageError, rd.w), red}})
	case s.NoLimitData && len(r.Limits) == 0:
		rd.add(line{{fitText("no usage limits reported", rd.w), dim}})
	}
}

func (rd *renderer) breakdown(b *report.Breakdown) {
	items := make([]string, 0, len(b.Rows))
	for _, row := range b.Rows {
		items = append(items, row.Name+" "+Percent(row.Percent))
	}
	// Prefer showing every surface with a shorter prefix over dropping
	// surfaces.
	for n := len(items); n > 0; n-- {
		text := strings.Join(items[:n], " · ")
		if n < len(items) {
			text += " · …"
		}
		for _, prefix := range []string{"By surface: ", "7d: ", ""} {
			if cellWidth(prefix)+cellWidth(text) <= rd.w {
				l := line{}
				if prefix != "" {
					l = append(l, seg{prefix, dim})
				}
				rd.add(append(l, seg{text, plain}))
				return
			}
		}
	}
	rd.add(line{{fitText(strings.Join(items, " · "), rd.w), plain}})
}

func (rd *renderer) spend(s *report.Spend) {
	used := Money(s.Used, s.Currency)
	var variants []string
	if s.Limit != nil {
		lim := Money(*s.Limit, s.Currency)
		pct := ""
		if s.Percent != nil {
			pct = " · " + Percent(*s.Percent)
		}
		variants = []string{
			"Extra usage: " + used + " / " + lim + pct,
			"Extra: " + used + "/" + lim + pct,
			"Extra: " + used + "/" + lim,
		}
	} else {
		variants = []string{"Extra usage: " + used + " spent", "Extra: " + used}
	}
	for _, v := range variants {
		if cellWidth(v) <= rd.w {
			rd.add(line{{v, sevStyle(s.Severity)}})
			return
		}
	}
	rd.add(line{{fitText(variants[len(variants)-1], rd.w), sevStyle(s.Severity)}})
}

func (rd *renderer) local(st *localstats.Stats, localErr string) {
	if rd.trim < trimLocalHeader {
		header := part{[]string{"Local Claude Code", "Local stats", "Local"}, dim | bold}
		rd.add(line{{header.bestFit(rd.w), header.st}})
	}
	if st.Today.Messages == 0 && st.Week.Messages == 0 {
		if localErr != "" {
			rd.add(line{{fitText("local stats unavailable: "+localErr, rd.w), red}})
			return
		}
		rd.add(line{{fitText("no activity in the last 7 days", rd.w), dim}})
		return
	}
	todayLabel, weekLabel, modelsLabel := "Today", "7 days", "Models"
	if st.WeekFromAPI {
		weekLabel = "Week"
	}
	labelW := max(cellWidth(todayLabel), cellWidth(weekLabel), cellWidth(modelsLabel)) + 1
	if rd.w < 20 {
		// Narrow: keep short labels so the two periods stay distinguishable;
		// the Models line is self-explanatory without one.
		todayLabel, weekLabel, modelsLabel = "Day", "7d", ""
		if st.WeekFromAPI {
			weekLabel = "Wk"
		}
		labelW = 4
	}
	period := func(label string, p localstats.Period) {
		items := []seg{
			{Number(p.Tokens.Total) + " tok", bold},
			{Number(p.Tokens.Output) + " out", plain},
			{Number(int64(p.Messages)) + " msg", plain},
			{fmt.Sprintf("%d sess", p.Sessions), plain},
		}
		rd.add(rd.labelled(label, labelW, items))
	}
	period(todayLabel, st.Today)
	period(weekLabel, st.Week)
	if rd.trim >= trimModels {
		return
	}
	var models []seg
	for i, m := range st.Week.Models {
		if i == 3 {
			break
		}
		models = append(models, seg{ModelName(m.Model) + " " + Percent(m.Share*100), plain})
	}
	if len(models) > 0 {
		lw := labelW
		if modelsLabel == "" {
			lw = 0
		}
		rd.add(rd.labelled(modelsLabel, lw, models))
	}
}

// labelled renders "Label  a · b · c", dropping trailing items that don't fit.
func (rd *renderer) labelled(label string, labelW int, items []seg) line {
	for n := len(items); n >= 1; n-- {
		l := line{}
		if labelW > 0 {
			l = append(l, seg{padRight(label, labelW), dim})
		}
		for i, it := range items[:n] {
			if i > 0 {
				l = append(l, seg{" · ", dim})
			}
			l = append(l, it)
		}
		if l.width() <= rd.w || n == 1 {
			return l
		}
	}
	return nil
}
