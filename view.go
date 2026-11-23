package main

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"ofam.dev/ochazuke/core"
	"ofam.dev/ochazuke/ui"
	"ofam.dev/ochazuke/widgets/picker"
	"ofam.dev/ochazuke/widgets/table"
)

// The page: a title line (the disk), the culprit list beside the focused
// culprit's detail (stacked below stackWidth columns), the filter line
// while typing, an info line and a status line. Modals draw over it.

// geom is where the list lands: its size, the rows it shows, and whether
// the detail pane sits beside it (else below).
type geom struct {
	tableW, tableH, rows int
	side                 bool
}

func (u *UI) geometry(p *Page, w, h int) geom {
	if w <= 0 || h <= 0 {
		w, h = 120, 32
	}
	body := h - 3
	if p.Typing {
		body--
	}
	body = max(body, 3)
	g := geom{tableW: w, tableH: body}
	if p.Detail {
		if w >= u.StackW {
			g.side = true
			g.tableW = int(math.Round(float64(max(0, w-1)) * p.Ratio))
		} else {
			g.tableH = int(math.Round(float64(max(0, body-1)) * p.Ratio))
		}
	}
	g.rows = max(g.tableH-2, 1)
	return g
}

func textW(s string) int { return utf8.RuneCountInString(s) }

func text(t, tone string) ui.ViewNode {
	return ui.ViewNode{Kind: ui.KindText, Tone: tone, Props: map[string]any{"text": t}}
}

func clip(s string, w int) string {
	if r := []rune(s); w > 1 && len(r) > w {
		return string(r[:w-1]) + "…"
	}
	return s
}

// view builds the whole screen.
func (u *UI) view(s core.State) ui.ViewNode {
	p := pageOf(s)
	w := s.Width
	if w <= 0 {
		w = 120
	}
	g := u.geometry(&p, s.Width, s.Height)
	grid := p.Grid.Copy()
	grid.PageSize = g.rows
	grid.Columns = make([]table.Column, len(gridColumns))
	for i, c := range gridColumns {
		if c.Key == p.Sort {
			if p.Desc {
				c.Title += " ▼"
			} else {
				c.Title += " ▲"
			}
		}
		grid.Columns[i] = c
	}
	body := grid.View(nodeGrid)
	body.Title = fmt.Sprintf("Culprits %d", len(p.Reports))
	if p.Detail {
		split := ui.ViewNode{ID: "body", Kind: ui.KindSplit, Ratio: p.Ratio, Gap: 1, Props: map[string]any{"axis": "horizontal"}}
		if !g.side {
			split.Props["axis"] = "vertical"
		}
		split.Children = []ui.ViewNode{body, u.detailNode(&p, &grid)}
		body = split
	}
	children := []ui.ViewNode{u.titleLine(&p, w), body}
	if p.Typing {
		children = append(children, p.inputNode())
	}
	children = append(children, p.infoLine(s, &grid, w), statusLine(&p, w))
	root := ui.ViewNode{ID: "root", Kind: ui.KindVStack, Children: []ui.ViewNode{
		{ID: "page", Kind: ui.KindVStack, Scope: "grid", Children: children},
	}}
	for _, m := range s.Modals {
		root.Children = append(root.Children, u.modalView(s, m))
	}
	return root
}

func (u *UI) titleLine(p *Page, w int) ui.ViewNode {
	var total int64
	for _, r := range p.Reports {
		total += r.Bytes
	}
	line := fmt.Sprintf(" o-df · Culprits %d · %s", len(p.Reports), Human(total))
	if p.Disk.Size > 0 {
		line += " · " + diskLine(p.Disk)
	}
	tone := "title"
	switch {
	case p.Loading:
		line += " · sizing…"
	case p.ScanErr != "":
		line += " · sizing failed"
		tone = "error"
	}
	if p.All {
		line += " · absent shown"
	}
	return text(clip(line, w), tone)
}

func (p *Page) inputNode() ui.ViewNode {
	line := p.QueryLine
	line.Sync(p.Query)
	return ui.ViewNode{ID: nodeFilter, Kind: ui.KindInput, Focusable: true, Title: "/ filter [" + string(p.Match) + "]", Props: map[string]any{
		"value": p.Query, "cursor": line.Cursor,
		"placeholder": "type to filter · enter keeps · esc clears · tab match mode · ↑↓ move · ctrl+a/e/w/u edit",
	}}
}

func (p *Page) infoLine(s core.State, grid *table.Model, w int) ui.ViewNode {
	var parts []string
	items := grid.Items()
	pos := "no rows"
	if len(items) > 0 {
		pos = fmt.Sprintf("row %d/%d", grid.SelectedIdx+1, len(items))
	}
	if len(grid.Rows) != len(p.Reports) {
		pos += fmt.Sprintf(" (%d of %d match)", len(grid.Rows), len(p.Reports))
	}
	parts = append(parts, pos, sortLabel(p))
	if n, bytes := p.markedBytes(); n > 0 {
		parts = append(parts, fmt.Sprintf("%d marked %s", n, Human(bytes)))
	}
	tone := "muted"
	if p.Query != "" || p.Typing {
		parts = append(parts, fmt.Sprintf("filter %s %q", p.Match, p.Query))
	}
	if p.FilterErr != "" {
		parts = append(parts, "regex error: "+p.FilterErr+" (no rows shown)")
		tone = "error"
	}
	if p.Grid.GroupField != "" {
		parts = append(parts, "grouped by "+p.Grid.GroupField)
	}
	if in := s.Input; in.Count != nil || len(in.Prefix) > 0 {
		pre := strings.Join(in.Prefix, " ")
		if in.Count != nil {
			pre = strconv.Itoa(*in.Count) + " " + pre
		}
		parts = append(parts, "pending "+strings.TrimSpace(pre))
	}
	return text(clip(" "+strings.Join(parts, " · "), w), tone)
}

func statusLine(p *Page, w int) ui.ViewNode {
	if p.Status != "" {
		return text(clip(" "+strings.ReplaceAll(p.Status, "\n", " "), w), p.Tone)
	}
	hint := " ? help · : commands · / filter · s sort · g b group · space mark · d d clean · D clean all · t/T trim · m relocate · r rescan · q quit"
	return text(clip(hint, w), "muted")
}

// detailNode shows the focused culprit's row and paths.
func (u *UI) detailNode(p *Page, grid *table.Model) ui.ViewNode {
	n := ui.ViewNode{ID: "detail", Kind: ui.KindPanel, Title: "Detail"}
	it, ok := grid.Focused()
	var body string
	switch {
	case !ok && p.Loading:
		body = "Sizing the culprits…"
	case !ok:
		body = "No culprit is present. a lists absent culprits too."
	case it.Header:
		n.Title = "Group"
		body = fmt.Sprintf("%s = %s\n%d culprit(s) · z a folds", p.Grid.GroupField, it.Group, it.Count)
	default:
		body = describe(u.App.Env, p.byName[it.Row.ID], u.App.Now())
	}
	t := text(body, "text")
	t.ID = detailText
	tail := text(" ", "text")
	tail.ID = detailTail
	n.Children = []ui.ViewNode{{ID: "detail.scroll", Kind: ui.KindScroll, Props: map[string]any{"offset": p.DetailOff}, Children: []ui.ViewNode{t, tail}}}
	return n
}

func window(textBody string, off int) string {
	lines := strings.Split(textBody, "\n")
	off = min(max(off, 0), len(lines))
	return strings.Join(lines[off:], "\n")
}

func (u *UI) modalView(s core.State, f core.Frame) ui.ViewNode {
	switch f.ID {
	case modalPalette:
		m, _ := f.Data.(*picker.Model)
		if m == nil {
			break
		}
		return ui.ViewNode{ID: "palette.overlay", Kind: ui.KindOverlay, Scope: "palette", Title: "Command palette · " + string(m.MatchMode),
			Props:    map[string]any{"widthPct": 70, "heightPct": 70},
			Children: []ui.ViewNode{m.View(modalPalette), text("enter runs · esc closes · tab match mode · ↑↓ move · ctrl+a/e/w/u edit", "muted")}}
	case modalHelp:
		off, _ := f.Data.(int)
		return ui.ViewNode{ID: modalHelp, Kind: ui.KindOverlay, Scope: "help", Focusable: true, Title: "Help · active key bindings",
			Props:    map[string]any{"widthPct": 80, "heightPct": 85},
			Children: []ui.ViewNode{text(window(strings.Join(u.helpLines(), "\n"), off), "text")}}
	case modalConfirm:
		d, _ := f.Data.(confirmData)
		return ui.ViewNode{ID: modalConfirm, Kind: ui.KindOverlay, Scope: "confirm", Focusable: true, Title: d.Title,
			Props: map[string]any{"widthPct": 70, "heightPct": 70},
			Children: []ui.ViewNode{text(window(d.Text, d.Off), "text"),
				text("y applies · n / esc cancels (nothing changes) · j/k scroll", "warning")}}
	case modalPreview:
		d, _ := f.Data.(previewData)
		return ui.ViewNode{ID: modalPreview, Kind: ui.KindOverlay, Scope: "preview", Focusable: true, Title: d.Title,
			Props:    map[string]any{"widthPct": 80, "heightPct": 75},
			Children: []ui.ViewNode{text(window(d.Text, d.Off), "text")}}
	case modalForm:
		rf, ok := f.Data.(relocateForm)
		if !ok {
			break
		}
		kids := []ui.ViewNode{rf.M.View(modalForm)}
		for _, sp := range rf.M.Specs {
			if e := rf.M.Errors[sp.Name]; e != "" {
				kids = append(kids, text(sp.Name+": "+e, "error"))
			}
		}
		kids = append(kids, text("type to edit (ctrl+a/e/w/u) · space flips yes/no · tab/↑↓ field · enter shows the dry run · esc cancels", "muted"))
		return ui.ViewNode{ID: "form.overlay", Kind: ui.KindOverlay, Scope: "form", Title: rf.M.Title, Props: map[string]any{"widthPct": 70}, Children: kids}
	}
	return ui.ViewNode{ID: f.ID + ".overlay", Kind: ui.KindOverlay, Children: []ui.ViewNode{text(f.ID, "muted")}}
}

// --- live help ---------------------------------------------------------------------

// keyHelp is one binding that actually resolves in its mode.
type keyHelp struct{ Keys, Command, Desc string }

var helpSections = []struct {
	mode, title string
	chain       []core.ContextFrame
}{
	{"normal", "Culprits", []core.ContextFrame{{Scope: "row"}, {Scope: "grid"}, {Scope: "app"}}},
	{"input", "Filter input (/)", []core.ContextFrame{{Scope: "grid"}, {Scope: "app"}}},
	{"palette", "Command palette (:)", []core.ContextFrame{{Scope: "palette"}, {Scope: "app"}}},
	{"confirm", "Confirm (dry run of d d, D, T, m)", []core.ContextFrame{{Scope: "confirm"}, {Scope: "app"}}},
	{"preview", "Trim dry run (t)", []core.ContextFrame{{Scope: "preview"}, {Scope: "app"}}},
	{"form", "Relocate form (m)", []core.ContextFrame{{Scope: "form"}, {Scope: "app"}}},
	{"help", "Help", []core.ContextFrame{{Scope: "help"}, {Scope: "app"}}},
}

var classSamples = map[string]string{core.ClassDigit0: "0", core.ClassDigit: "5", core.ClassAlpha: "x", core.ClassPrintable: "x", core.ClassPaste: core.PasteKey, core.ClassAny: "x"}

// resolveWinner runs keys through the keymap from an idle state and
// returns the binding that fires.
func resolveWinner(km *core.Keymap, chain []core.ContextFrame, mode string, keys []string) *core.Binding {
	in := core.InputState{}
	for i, k := range keys {
		res := km.Resolve(core.KeyQuery{Chain: chain, Mode: mode, Input: in, Key: k, Text: k})
		in = res.Input
		last := i == len(keys)-1
		switch {
		case !last && res.Outcome == core.Pending:
		case last && (res.Outcome == core.Matched || res.Outcome == core.PushedLayer):
			return res.Trace.Winner
		case last && res.Outcome == core.Pending && res.Trace.Winner != nil:
			return res.Trace.Winner
		default:
			return nil
		}
	}
	return nil
}

// activeBindings lists the bindings of mode that win resolution, so user
// overrides and unbinds show exactly as they behave.
func (u *UI) activeBindings(mode string, chain []core.ContextFrame) []keyHelp {
	var out []keyHelp
	seen := map[string]bool{}
	for _, bd := range u.Bindings {
		if bd.Hidden || bd.Unbind || (bd.Mode != mode && bd.Mode != "") || bd.Command == "" && bd.Layer == nil {
			continue
		}
		keys, label := bd.Keys, strings.Join(bd.Keys, " ")
		if len(keys) == 0 {
			sample, ok := classSamples[bd.Class]
			if !ok {
				continue
			}
			keys, label = []string{sample}, "‹"+bd.Class+"›"
			if bd.Class == core.ClassDigit0 {
				label = "0"
			}
		}
		if seen[label] {
			continue
		}
		win := resolveWinner(u.Km, chain, mode, keys)
		if win == nil || win.Command != bd.Command || !slices.Equal(win.Keys, bd.Keys) || win.Class != bd.Class {
			continue
		}
		seen[label] = true
		desc := bd.Hint
		if spec, ok := u.Km.Commands.Lookup(bd.Command); ok && spec.Description != "" {
			desc = spec.Description
		}
		out = append(out, keyHelp{Keys: label, Command: bd.Command, Desc: desc})
	}
	return out
}

// keysByCommand maps command IDs to their active keys in mode.
func keysByCommand(u *UI, mode string) map[string][]string {
	out := map[string][]string{}
	for _, sec := range helpSections {
		if sec.mode != mode {
			continue
		}
		for _, k := range u.activeBindings(mode, sec.chain) {
			out[k.Command] = append(out[k.Command], k.Keys)
		}
	}
	return out
}

// helpLines renders the help page from the live keymap.
func (u *UI) helpLines() []string {
	var lines []string
	for _, sec := range helpSections {
		bs := u.activeBindings(sec.mode, sec.chain)
		if len(bs) == 0 {
			continue
		}
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, sec.title)
		for _, k := range bs {
			lines = append(lines, fmt.Sprintf("  %-12s %s  (%s)", k.Keys, k.Desc, k.Command))
		}
	}
	lines = append(lines, "",
		"Nothing is removed without the confirm: d d, D, T and m show the dry run; only y applies it.",
		"Counts prefix motions: 10j, 5G. Marked culprits (space) are what d d cleans.",
		"bindings.json, themes.json and settings.json load from the culprits file's directory (~/.config/o-df).")
	for _, n := range u.Notes {
		lines = append(lines, "config: "+n)
	}
	return lines
}
