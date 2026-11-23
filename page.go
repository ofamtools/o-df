package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"

	"ofam.dev/ochazuke/config"
	"ofam.dev/ochazuke/core"
	otea "ofam.dev/ochazuke/tea"
	"ofam.dev/ochazuke/ui"
	"ofam.dev/ochazuke/widgets/form"
	"ofam.dev/ochazuke/widgets/input"
	"ofam.dev/ochazuke/widgets/picker"
	"ofam.dev/ochazuke/widgets/table"
)

// The culprits page on Ochazuke v2: one page frame whose Data is a Page
// value, modal frames for the palette, help, the relocate form, the
// confirm and the dry-run preview. Every key is a binding row (user rows
// from bindings.json go first), every action a command handler; sizing,
// cleaning, trimming and relocating run as async effects, so the page never
// blocks. Nothing destructive runs without a confirm modal: the modal shows
// the dry run (what would go, how big) and only y applies it.

// Frame and node IDs.
const (
	pageID       = "main"
	nodeGrid     = "grid"
	nodeFilter   = "filter"
	modalPalette = "palette"
	modalHelp    = "help"
	modalConfirm = "confirm"
	modalPreview = "preview"
	modalForm    = "form"
	detailText   = "detail.text"
	detailTail   = "detail.tail"
)

// sortKeys are what s cycles through; groupKeys what g b cycles through.
var (
	sortKeys  = []string{"size", "name", "used", "category", "advice"}
	groupKeys = []string{"", "category", "advice", "rebuild"}
	densities = []string{ui.DensityBalanced, ui.DensityCompact, ui.DensitySpacious}
)

// Page is the page state, kept by value in the page frame's Data. Handlers
// copy it, change the copy and store it back.
type Page struct {
	Reports []Report
	Disk    Disk
	Loading bool
	Loaded  bool
	All     bool // list absent culprits too
	ScanErr string

	base   []table.Row
	byName map[string]Report
	Grid   table.Model

	Query     string
	QueryLine input.Line
	Typing    bool
	Match     table.MatchMode
	FilterErr string

	Sort string // one of sortKeys
	Desc bool

	Detail    bool
	Ratio     float64
	DetailOff int
	detailRow string

	ThemeIdx int
	Density  string

	Status, Tone string
}

func (p Page) clone() Page {
	p.Grid = p.Grid.Copy()
	p.QueryLine.Text = slices.Clone(p.QueryLine.Text)
	return p
}

func pageOf(s core.State) Page {
	p, _ := s.Pages[pageID].Data.(Page)
	return p
}

func withPage(s core.State, p Page) core.State {
	f := s.Pages[pageID]
	f.Data = p
	s.Pages = maps.Clone(s.Pages)
	s.Pages[pageID] = f
	return s
}

func (p *Page) note(msg string) { p.Status, p.Tone = msg, "success" }
func (p *Page) info(msg string) { p.Status, p.Tone = msg, "info" }
func (p *Page) warn(msg string) { p.Status, p.Tone = msg, "warning" }
func (p *Page) fail(msg string) { p.Status, p.Tone = msg, "error" }

var gridColumns = []table.Column{
	{Key: "name", Title: "culprit"},
	{Key: "size", Title: "size", Align: "right"},
	{Key: "rebuild", Title: "rebuild"},
	{Key: "used", Title: "used", Align: "right"},
	{Key: "advice", Title: "advice"},
	{Key: "category", Title: "category"},
	{Key: "path", Title: "path"},
}

// rebuild recreates the base rows from the reports.
func (p *Page) rebuild(ui *UI) {
	p.base = make([]table.Row, len(p.Reports))
	p.byName = make(map[string]Report, len(p.Reports))
	for i, r := range p.Reports {
		c := ui.App.cells(r)
		p.base[i] = table.Row{ID: r.Name, Ref: &core.Ref{Kind: CulpritKind, ID: r.Name}, Cells: map[string]any{
			"name": c[0], "size": c[1], "rebuild": c[2], "used": c[3], "advice": c[4], "path": c[5], "category": r.Category,
		}}
		p.byName[r.Name] = r
	}
	p.Grid.Columns = gridColumns
	for id := range p.Grid.Marked {
		if _, ok := p.byName[id]; !ok {
			delete(p.Grid.Marked, id)
		}
	}
}

// reproject filters (the table widget's matcher over every cell), sorts
// (sizes and ages by value, the rest by text; stable) and keeps the cursor
// on the same culprit when it is still shown.
func (p *Page) reproject() {
	keep, onRow := p.focusedName()
	tmp := table.Model{Columns: gridColumns, Rows: p.base, Query: p.Query, MatchMode: p.Match}
	rows := tmp.Filtered()
	p.FilterErr = tmp.FilterErr
	if p.FilterErr != "" {
		rows = nil
	}
	rows = slices.Clone(rows)
	slices.SortStableFunc(rows, func(x, y table.Row) int {
		c := p.compare(x.ID, y.ID)
		if p.Desc {
			return -c
		}
		return c
	})
	p.Grid.Rows = rows
	if onRow {
		p.focusName(keep)
	}
	p.Grid.Focused()
}

func (p *Page) compare(x, y string) int {
	a, b := p.byName[x], p.byName[y]
	switch p.Sort {
	case "size":
		return cmpInt(a.Bytes, b.Bytes)
	case "used":
		return a.LastUsed.Compare(b.LastUsed)
	case "category":
		return strings.Compare(a.Category, b.Category)
	case "advice":
		return strings.Compare(a.Recommend, b.Recommend)
	}
	return strings.Compare(a.Name, b.Name)
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (p *Page) focusedName() (string, bool) {
	r, ok := p.Grid.FocusedRow()
	return r.ID, ok
}

func (p *Page) focusName(id string) bool {
	for i, it := range p.Grid.Items() {
		if !it.Header && it.Row.ID == id {
			p.Grid.SelectedIdx = i
			p.Grid.Focused()
			return true
		}
	}
	return false
}

// targets are the marked culprits (in name order), else the focused one.
func (p *Page) targets() []string {
	var out []string
	for id := range p.Grid.Marked {
		if _, ok := p.byName[id]; ok {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		if id, ok := p.focusedName(); ok {
			out = []string{id}
		}
	}
	return out
}

func (p *Page) markedBytes() (int, int64) {
	n, total := 0, int64(0)
	for id := range p.Grid.Marked {
		if r, ok := p.byName[id]; ok {
			n++
			total += r.Bytes
		}
	}
	return n, total
}

// --- environment -----------------------------------------------------------------

// UI is the immutable page configuration shared by handlers and the view.
type UI struct {
	App      *App
	Km       *core.Keymap
	Bindings []core.Binding
	Specs    []core.CommandSpec
	Themes   []ui.Theme
	Match    table.MatchMode
	Ratio    float64
	StackW   int
	Detail   bool
	Density  string
	All      bool
	Notes    []string
}

// UIConfig is what main (or a test) supplies to build a UI.
type UIConfig struct {
	Bindings []core.Binding
	Theme    *ui.Theme
	Settings *config.Settings
	All      bool
	Notes    []string
}

// defaultSettings are the recognized settings.json keys and their defaults.
var defaultSettings = map[string]any{
	"matchMode":   "substring", // filter and palette match mode: substring, fuzzy or regex
	"detail":      true,        // show the detail pane at startup (enter toggles it)
	"detailRatio": 0.6,         // list share of the detail split, 0.2 to 0.8
	"stackWidth":  100,         // below this terminal width the detail pane stacks under the list
	"density":     "balanced",  // compact, balanced or spacious (g d cycles)
}

var modes = []core.Mode{{Name: "normal"}, {Name: "input"}, {Name: "palette"}, {Name: "help"}, {Name: "confirm"}, {Name: "preview"}, {Name: "form"}}

func newUI(app *App, cfg UIConfig) *UI {
	u := &UI{App: app, All: cfg.All, Notes: cfg.Notes}
	base := ui.DefaultDarkTheme()
	if cfg.Theme != nil {
		base = *cfg.Theme
	}
	u.Themes = []ui.Theme{base, ui.DefaultLightTheme(), ui.HighContrastDarkTheme()}
	set := cfg.Settings
	if set == nil {
		set, _ = config.LoadSettings(nil, defaultSettings)
	}
	switch m := table.MatchMode(set.String("matchMode", "substring")); m {
	case table.MatchSubstring, table.MatchFuzzy, table.MatchRegex:
		u.Match = m
	default:
		u.Match = table.MatchSubstring
		u.Notes = append(u.Notes, "settings: matchMode must be substring, fuzzy or regex")
	}
	u.Ratio = min(max(set.Float("detailRatio", 0.6), 0.2), 0.8)
	u.StackW = set.Int("stackWidth", 100)
	u.Detail = set.Bool("detail", true)
	u.Density = set.String("density", ui.DensityBalanced)
	if !slices.Contains(densities, u.Density) {
		u.Notes = append(u.Notes, "settings: density must be compact, balanced or spacious")
		u.Density = ui.DensityBalanced
	}
	u.Specs = commandSpecs()
	for _, d := range config.CheckBindings(modes, cfg.Bindings, u.Specs...) {
		if d.Severity == config.SevError {
			u.Notes = append(u.Notes, fmt.Sprintf("bindings.json #%d: %s", d.Index, d.Message))
		}
	}
	u.Bindings = config.MergeBindings(defaultBindings(), cfg.Bindings)
	km, err := core.NewKeymap(modes, u.Bindings, u.Specs...)
	if err != nil {
		u.Notes = append(u.Notes, "bindings: "+strings.ReplaceAll(err.Error(), "\n", "; "))
		if km == nil {
			km, _ = core.NewKeymap(modes, defaultBindings(), u.Specs...)
		}
	}
	u.Km = km
	return u
}

// theme is the page's theme with its density.
func (u *UI) theme(p *Page) ui.Theme {
	t := u.Themes[p.ThemeIdx%len(u.Themes)]
	t.Density = p.Density
	return t
}

// --- commands and bindings ----------------------------------------------------------

// commandSpecs are the page's commands. Those tagged "user" are listed in
// the palette; the others are plumbing for inputs, modals and effects.
func commandSpecs() []core.CommandSpec {
	user := func(id, desc string) core.CommandSpec {
		return core.CommandSpec{ID: id, Description: desc, Tags: []string{"user"}}
	}
	internal := func(id, desc string) core.CommandSpec { return core.CommandSpec{ID: id, Description: desc} }
	specs := []core.CommandSpec{
		user("filter.open", "Filter the culprits (every column)"),
		user("filter.clear", "Clear the filter"),
		user("filter.mode.cycle", "Cycle the filter match mode: substring, fuzzy, regex"),
		user("sort.cycle", "Sort by the next field: size, name, used, category, advice"),
		user("sort.reverse", "Reverse the sort order"),
		user("group.cycle", "Group by the next field: none, category, advice, rebuild"),
		user("group.collapse", "Collapse or expand the focused group"),
		user("mark.toggle", "Mark or unmark the focused culprit"),
		user("mark.clear", "Unmark every culprit"),
		user("detail.toggle", "Show or hide the detail pane"),
		user("detail.scroll.down", "Scroll the detail pane down"),
		user("detail.scroll.up", "Scroll the detail pane up"),
		user("split.grow", "Widen the list next to the detail pane"),
		user("split.shrink", "Narrow the list next to the detail pane"),
		user("split.reset", "Reset the detail split"),
		user("culprit.refresh", "Size the culprits again (async)"),
		user("culprit.all.toggle", "List absent culprits too, or only present ones"),
		user("culprit.clean", "Clean the marked culprits (else the focused one): shows the dry run, y applies"),
		user("culprit.clean.all", "Clean every present rebuildable culprit: shows the dry run, y applies"),
		user("culprit.trim.preview", "Dry run of the focused culprit's retention rule: list what trim would remove"),
		user("culprit.trim", "Trim the focused culprit by its retention rule: shows the dry run, y applies"),
		user("culprit.relocate", "Relocate the focused culprit (form, then the dry run; y applies)"),
		user("theme.cycle", "Cycle the theme: dark, light, high contrast"),
		user("density.cycle", "Cycle the density: balanced, compact, spacious"),
		user("palette.open", "Open the command palette"),
		user("help.open", "Show the active key bindings"),
		user("app.close", "Quit"),

		internal("culprit.clean.apply", "Clean after the confirm (names or all)"),
		internal("culprit.trim.apply", "Trim after the confirm"),
		internal("culprit.relocate.apply", "Relocate after the confirm"),
		internal("scan.done", "Sizing finished"),
		internal("trim.planned", "A trim dry run finished"),
		internal("apply.done", "A change finished"),
		internal("input.insert", "Type into the filter"),
		internal("input.backspace", "Delete the last filter character"),
		internal("input.accept", "Keep the filter and return to the list"),
		internal("input.cancel", "Clear the filter and return to the list"),
		internal("palette.insert", "Type into the palette query"),
		internal("palette.backspace", "Delete the last query character"),
		internal("palette.close", "Close the palette"),
		internal(picker.CmdConfirm, "Run the highlighted command"),
		internal(picker.CmdModeCycle, "Cycle the match mode"),
		internal("help.close", "Close help"),
		internal("confirm.yes", "Apply: run what the dry run shows"),
		internal("confirm.no", "Cancel: change nothing"),
		internal("preview.close", "Close the dry run"),
		internal(form.CmdInsert, "Type into the field"),
		internal(form.CmdBackspace, "Delete the last character"),
		internal(form.CmdToggle, "Toggle a yes/no field"),
		internal(form.CmdSubmit, "Submit the form"),
		internal(form.CmdCancel, "Discard the form"),
	}
	return append(specs, input.ReadlineSpecs()...)
}

func b(mode, keys, cmd, hint string) core.Binding {
	return core.Binding{Mode: mode, Keys: strings.Fields(keys), Command: cmd, Hint: hint}
}

func class(mode, cls, cmd string, args map[string]any, hint string) core.Binding {
	return core.Binding{Mode: mode, Class: cls, Command: cmd, Args: args, Hint: hint}
}

// unusedMotions are standard motions a one-axis list has no use for.
var unusedMotions = []string{"motion.left", "motion.right", "motion.start", "motion.end", "motion.word.next", "motion.word.prev", "motion.word.end", "motion.group.next", "motion.group.prev"}

func standard(mode string) []core.Binding {
	var out []core.Binding
	for _, bd := range core.StandardBindings(mode) {
		if !slices.Contains(unusedMotions, bd.Command) {
			out = append(out, bd)
		}
	}
	return out
}

// defaultBindings is the built-in keymap. Help is generated from whatever
// of it (plus user overrides) actually resolves.
func defaultBindings() []core.Binding {
	text := map[string]any{"text": "$text"}
	out := []core.Binding{
		b("normal", "/", "filter.open", "filter"),
		b("normal", "esc", "filter.clear", "clear filter"),
		b("normal", "alt+m", "filter.mode.cycle", "match mode"),
		b("normal", "s", "sort.cycle", "sort"),
		b("normal", "S", "sort.reverse", "reverse sort"),
		b("normal", "g b", "group.cycle", "group"),
		b("normal", "z a", "group.collapse", "fold group"),
		b("normal", "space", "mark.toggle", "mark"),
		b("normal", "M", "mark.clear", "unmark all"),
		b("normal", "enter", "detail.toggle", "detail"),
		b("normal", "J", "detail.scroll.down", "scroll detail down"),
		b("normal", "K", "detail.scroll.up", "scroll detail up"),
		b("normal", ">", "split.grow", "wider list"),
		b("normal", "<", "split.shrink", "narrower list"),
		b("normal", "=", "split.reset", "reset split"),
		b("normal", "R", "culprit.refresh", "rescan"),
		b("normal", "r", "culprit.refresh", "rescan"),
		b("normal", "a", "culprit.all.toggle", "absent too"),
		b("normal", "d d", "culprit.clean", "clean"),
		b("normal", "D", "culprit.clean.all", "clean all"),
		b("normal", "t", "culprit.trim.preview", "trim dry run"),
		b("normal", "T", "culprit.trim", "trim"),
		b("normal", "m", "culprit.relocate", "relocate"),
		b("normal", "g t", "theme.cycle", "theme"),
		b("normal", "g d", "density.cycle", "density"),
		b("normal", ":", "palette.open", "commands"),
		b("normal", "?", "help.open", "help"),
		b("normal", "q", "app.close", "quit"),
		b("normal", "ctrl+c", "app.close", "quit"),
	}
	out = append(out, standard("normal")...)
	out = append(out,
		class("input", core.ClassPrintable, "input.insert", text, "type"),
		class("input", core.ClassPaste, "input.insert", text, "paste"),
		b("input", "backspace", "input.backspace", "delete"),
		b("input", "enter", "input.accept", "keep filter"),
		b("input", "esc", "input.cancel", "clear filter"),
		b("input", "ctrl+c", "input.cancel", "clear filter"),
		b("input", "alt+m", "filter.mode.cycle", "match mode"),
		b("input", "tab", "filter.mode.cycle", "match mode"),
		b("input", "down", "motion.down", "next row"),
		b("input", "ctrl+n", "motion.down", "next row"),
		b("input", "up", "motion.up", "previous row"),
		b("input", "ctrl+p", "motion.up", "previous row"),

		class("palette", core.ClassPrintable, "palette.insert", text, "type"),
		class("palette", core.ClassPaste, "palette.insert", text, "paste"),
		b("palette", "backspace", "palette.backspace", "delete"),
		b("palette", "enter", picker.CmdConfirm, "run"),
		b("palette", "esc", "palette.close", "close"),
		b("palette", "ctrl+c", "palette.close", "close"),
		b("palette", "alt+m", picker.CmdModeCycle, "match mode"),
		b("palette", "tab", picker.CmdModeCycle, "match mode"),
		b("palette", "down", "motion.down", "next"),
		b("palette", "ctrl+n", "motion.down", "next"),
		b("palette", "up", "motion.up", "previous"),
		b("palette", "ctrl+p", "motion.up", "previous"),

		class("form", core.ClassPrintable, form.CmdInsert, text, "type"),
		class("form", core.ClassPaste, form.CmdInsert, text, "paste"),
		b("form", "backspace", form.CmdBackspace, "delete"),
		b("form", "tab", "motion.down", "next field"),
		b("form", "down", "motion.down", "next field"),
		b("form", "shift+tab", "motion.up", "previous field"),
		b("form", "up", "motion.up", "previous field"),
		b("form", "enter", form.CmdSubmit, "continue"),
		b("form", "esc", form.CmdCancel, "cancel"),
		b("form", "ctrl+c", form.CmdCancel, "cancel"),

		b("confirm", "y", "confirm.yes", "apply"),
		b("confirm", "Y", "confirm.yes", "apply"),
		b("confirm", "n", "confirm.no", "cancel"),
		b("confirm", "N", "confirm.no", "cancel"),
		b("confirm", "esc", "confirm.no", "cancel"),
		b("confirm", "q", "confirm.no", "cancel"),
		b("confirm", "ctrl+c", "confirm.no", "cancel"),

		b("preview", "esc", "preview.close", "close"),
		b("preview", "q", "preview.close", "close"),
		b("preview", "enter", "preview.close", "close"),
		b("preview", "ctrl+c", "preview.close", "close"),
		b("preview", "T", "culprit.trim", "trim (asks)"),

		b("help", "esc", "help.close", "close"),
		b("help", "q", "help.close", "close"),
		b("help", "?", "help.close", "close"),
		b("help", "ctrl+c", "help.close", "close"),
	)
	for _, mode := range []string{"input", "palette", "form"} {
		taken := map[string]bool{}
		for _, bd := range out {
			if bd.Mode == mode && len(bd.Keys) > 0 {
				taken[strings.Join(bd.Keys, " ")] = true
			}
		}
		for _, rb := range input.ReadlineBindings(mode, "") {
			if k := strings.Join(rb.Keys, " "); !taken[k] {
				taken[k] = true
				out = append(out, rb)
			}
		}
	}
	for _, mode := range []string{"help", "confirm", "preview"} {
		out = append(out, standard(mode)...)
	}
	return out
}

// --- engine ----------------------------------------------------------------------------

type fn func(s core.State, p *Page, c core.Command) (core.State, []core.Effect)

// wrap turns a page handler into a core.Handler: it copies the Page,
// clears the last status on a person's command (an effect's result keeps
// it), sizes the grid, runs f and stores the Page back.
func (u *UI) wrap(f fn) core.Handler {
	return func(s core.State, c core.Command) core.StepResult {
		p := pageOf(s).clone()
		if c.Actor.Kind != "system" {
			p.Status, p.Tone = "", ""
		}
		p.Grid.PageSize = u.geometry(&p, s.Width, s.Height).rows
		s, effects := f(s, &p, c)
		if !p.Detail {
			p.DetailOff = 0
		} else if id, _ := p.focusedName(); id != p.detailRow {
			p.detailRow, p.DetailOff = id, 0
		}
		return core.Next(withPage(s, p), effects...)
	}
}

func setPage(s core.State, mode, focus string) core.State {
	f := s.Pages[pageID]
	f.Mode, f.FocusID = mode, focus
	s.Pages = maps.Clone(s.Pages)
	s.Pages[pageID] = f
	return s
}

func openModal(s core.State, id, mode string, data any) core.State {
	s = closeModal(s, id)
	s.Modals = append(slices.Clone(s.Modals), core.Frame{ID: id, Kind: core.FrameModal, Mode: mode, FocusID: id, Data: data})
	return s
}

func closeModal(s core.State, id string) core.State {
	if i := slices.IndexFunc(s.Modals, func(f core.Frame) bool { return f.ID == id }); i >= 0 {
		s.Modals = slices.Delete(slices.Clone(s.Modals), i, i+1)
	}
	return s
}

func modalData(s core.State, id string) any {
	if i := slices.IndexFunc(s.Modals, func(f core.Frame) bool { return f.ID == id }); i >= 0 {
		return s.Modals[i].Data
	}
	return nil
}

func setModalData(s core.State, id string, data any) core.State {
	if i := slices.IndexFunc(s.Modals, func(f core.Frame) bool { return f.ID == id }); i >= 0 {
		s.Modals = slices.Clone(s.Modals)
		s.Modals[i].Data = data
	}
	return s
}

// dispatch asks the shell to run cmd through the engine after this step
// (palette picks, confirmed actions), so it meets the same handlers as a key.
func dispatch(c core.Command) core.Effect { return core.CustomEffect{Kind: "dispatch", Data: c} }

// confirmData is the confirm modal: the dry run's text and the command y runs.
type confirmData struct {
	Title, Text string
	Off         int
	Action      core.Command
}

// previewData is a read-only dry run (trim's list).
type previewData struct {
	Title, Text string
	Off         int
}

// relocateForm is the relocate form and the culprit it moves.
type relocateForm struct {
	Name string
	M    *form.Model
}

// scanDone and trimPlanned are effect values.
type scanDone struct {
	Reports []Report
	Disk    Disk
}

type trimPlanned struct {
	Plan  TrimResult
	Apply bool
}

func confirmed(c core.Command) bool { v, _ := c.Args["confirmed"].(bool); return v }

func newEngine(u *UI) *core.Engine {
	e := core.NewEngine(u.Km)
	h := func(id string, f fn) { e.Handle(id, u.wrap(f)) }
	scoped := func(scope, id string, f fn) { e.HandleScope(scope, id, u.wrap(f)) }
	app := u.App

	for _, id := range []string{"motion.down", "motion.up", "motion.top", "motion.bottom", "motion.page.down", "motion.page.up", "motion.half.down", "motion.half.up", "motion.block.next", "motion.block.prev"} {
		scoped("grid", id, func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
			if (c.ID == "motion.bottom" || c.ID == "motion.top") && c.Count > 0 {
				p.Grid.SelectedIdx = c.Count - 1
				p.Grid.Focused()
				return s, nil
			}
			p.Grid.HandleCommand(c)
			return s, nil
		})
		scoped("palette", id, u.paletteCmd)
		scoped("help", id, u.scrollModal(modalHelp))
		scoped("confirm", id, u.scrollModal(modalConfirm))
		scoped("preview", id, u.scrollModal(modalPreview))
		scoped("form", id, u.formCmd)
	}

	// --- filter
	h("filter.open", func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		p.Typing = true
		return setPage(s, "input", nodeFilter), nil
	})
	h("filter.mode.cycle", func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		m := table.Model{MatchMode: p.Match}
		m.CycleMatchMode()
		p.Match = m.MatchMode
		p.reproject()
		p.info("match mode: " + string(p.Match))
		return s, nil
	})
	h("filter.clear", func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		if p.Query != "" {
			p.Query = ""
			p.reproject()
			p.info("filter cleared")
		}
		return s, nil
	})
	editQuery := func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		p.QueryLine.Sync(p.Query)
		changed := false
		switch c.ID {
		case "input.insert":
			changed = p.QueryLine.Insert(c.Arg("text"))
		case "input.backspace":
			changed, _ = p.QueryLine.HandleCommand(core.Command{ID: input.CmdBackspace})
		default:
			changed, _ = p.QueryLine.HandleCommand(c)
		}
		if changed {
			p.Query = p.QueryLine.Value()
			p.reproject()
		}
		return s, nil
	}
	h("input.insert", editQuery)
	h("input.backspace", editQuery)
	for _, id := range input.ReadlineCommandIDs() {
		h(id, editQuery)
	}
	h("input.accept", func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		p.Typing = false
		return setPage(s, "normal", nodeGrid), nil
	})
	h("input.cancel", func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		p.Typing, p.Query = false, ""
		p.reproject()
		return setPage(s, "normal", nodeGrid), nil
	})

	// --- sort, group, marks, detail
	h("sort.cycle", func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		i := slices.Index(sortKeys, p.Sort)
		p.Sort = sortKeys[(i+1)%len(sortKeys)]
		p.Desc = p.Sort == "size" || p.Sort == "used" // biggest, newest first
		p.reproject()
		p.info(sortLabel(p))
		return s, nil
	})
	h("sort.reverse", func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		p.Desc = !p.Desc
		p.reproject()
		p.info(sortLabel(p))
		return s, nil
	})
	h("group.cycle", func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		keep, onRow := p.focusedName()
		i := slices.Index(groupKeys, p.Grid.GroupField)
		p.Grid.GroupField = groupKeys[(i+1)%len(groupKeys)]
		p.Grid.Collapsed = nil
		if !onRow || !p.focusName(keep) {
			p.Grid.SelectedIdx = 0
		}
		p.Grid.Focused()
		if p.Grid.GroupField == "" {
			p.info("ungrouped")
		} else {
			p.info("grouped by " + p.Grid.GroupField)
		}
		return s, nil
	})
	h("group.collapse", func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		if p.Grid.GroupField == "" {
			p.warn("not grouped — g b groups")
			return s, nil
		}
		p.Grid.ToggleGroup()
		return s, nil
	})
	h("mark.toggle", func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		p.Grid.ToggleMark()
		p.Grid.HandleCommand(core.Command{ID: "motion.down"})
		return s, nil
	})
	h("mark.clear", func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		p.Grid.ClearMarks()
		p.info("marks cleared")
		return s, nil
	})
	h("detail.toggle", func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		p.Detail = !p.Detail
		return s, nil
	})
	scroll := func(dir int) fn {
		return func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
			if !p.Detail {
				p.info("the detail pane is hidden; enter shows it")
				return s, nil
			}
			if _, atEnd := s.Layout.Rects[detailTail]; dir < 0 {
				p.DetailOff = max(p.DetailOff-c.Times(), 0)
			} else if !atEnd {
				p.DetailOff += c.Times()
			}
			return s, nil
		}
	}
	h("detail.scroll.down", scroll(1))
	h("detail.scroll.up", scroll(-1))
	resize := func(d float64) fn {
		return func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
			if d == 0 {
				p.Ratio = u.Ratio
			} else {
				p.Ratio = min(max(p.Ratio+d*float64(c.Times()), 0.2), 0.8)
			}
			return s, nil
		}
	}
	h("split.grow", resize(0.05))
	h("split.shrink", resize(-0.05))
	h("split.reset", resize(0))

	// --- appearance
	h("theme.cycle", func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		p.ThemeIdx = (p.ThemeIdx + 1) % len(u.Themes)
		th := u.theme(p)
		p.info("theme: " + th.Name)
		return s, []core.Effect{core.CustomEffect{Kind: "theme", Data: th}}
	})
	h("density.cycle", func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		i := slices.Index(densities, p.Density)
		p.Density = densities[(i+1)%len(densities)]
		p.info("density: " + p.Density)
		return s, []core.Effect{core.CustomEffect{Kind: "theme", Data: u.theme(p)}}
	})

	// --- sizing
	h("culprit.refresh", func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		return s, []core.Effect{u.scanEffect(p)}
	})
	h("culprit.all.toggle", func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		p.All = !p.All
		if p.All {
			p.info("listing absent culprits too")
		} else {
			p.info("listing present culprits only")
		}
		return s, []core.Effect{u.scanEffect(p)}
	})
	h("scan.done", func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		p.Loading = false
		if err, _ := c.Args["error"].(error); err != nil {
			p.ScanErr = err.Error()
			p.fail("sizing failed: " + err.Error())
			return s, nil
		}
		res, _ := c.Args["value"].(scanDone)
		p.Reports, p.Disk, p.ScanErr, p.Loaded = res.Reports, res.Disk, "", true
		p.rebuild(u)
		p.reproject()
		if p.Status == "" {
			p.info(fmt.Sprintf("sized %s", culprits(len(p.Reports))))
		}
		return s, nil
	})

	// --- clean
	h("culprit.clean", func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		names := p.targets()
		if len(names) == 0 {
			p.warn("no culprit under the cursor")
			return s, nil
		}
		plan, err := app.planClear(context.Background(), names, false, p.Reports)
		if err != nil {
			p.fail(err.Error())
			return s, nil
		}
		if len(plan.Clear) == 0 {
			p.warn("nothing to clean: none of " + strings.Join(names, ", ") + " is present")
			return s, nil
		}
		return openModal(s, modalConfirm, "confirm", confirmData{Title: "Clean · dry run", Text: plan.Prompt(),
			Action: core.Command{ID: "culprit.clean.apply", Args: map[string]any{"names": names}}}), nil
	})
	h("culprit.clean.all", func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		plan, err := app.planClear(context.Background(), nil, true, p.Reports)
		if err != nil {
			p.fail(err.Error())
			return s, nil
		}
		if len(plan.Clear) == 0 {
			p.warn("nothing to clean: no rebuildable culprit is present")
			return s, nil
		}
		return openModal(s, modalConfirm, "confirm", confirmData{Title: "Clean all · dry run", Text: plan.Prompt(),
			Action: core.Command{ID: "culprit.clean.apply", Args: map[string]any{"all": true}}}), nil
	})
	h("culprit.clean.apply", func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		if !confirmed(c) {
			p.warn("cleaning needs the confirm: d d (or D) shows the dry run first")
			return s, nil
		}
		names, _ := c.Args["names"].([]string)
		all, _ := c.Args["all"].(bool)
		known := slices.Clone(p.Reports)
		p.info("cleaning…")
		return s, []core.Effect{core.AsyncEffect{Slot: "apply", Command: "apply.done", Run: func(ctx context.Context) (any, error) {
			plan, err := app.planClear(ctx, names, all, known)
			if err != nil {
				return nil, err
			}
			return clearManyNote(app.clearMany(ctx, plan)), nil
		}}}
	})

	// --- trim
	trimStart := func(apply bool) fn {
		return func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
			s = closeModal(s, modalPreview)
			id, ok := p.focusedName()
			if !ok {
				p.warn("no culprit under the cursor")
				return s, nil
			}
			cu := p.byName[id].Culprit
			if !cu.Trimmable() {
				p.warn(cu.Name + " has no retention rule (keep or days in the culprits file); trim has nothing to do")
				return s, nil
			}
			p.info("planning the trim of " + cu.Name + "…")
			env := app.Env
			return s, []core.Effect{core.AsyncEffect{Slot: "trim", Command: "trim.planned", Run: func(ctx context.Context) (any, error) {
				plan, err := PlanTrim(ctx, env, cu)
				return trimPlanned{Plan: plan, Apply: apply}, err
			}}}
		}
	}
	h("culprit.trim.preview", trimStart(false))
	h("culprit.trim", trimStart(true))
	h("trim.planned", func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		if err, _ := c.Args["error"].(error); err != nil {
			p.fail("trim: " + err.Error())
			return s, nil
		}
		tp, _ := c.Args["value"].(trimPlanned)
		plan := tp.Plan
		if !tp.Apply {
			p.Status = ""
			return openModal(s, modalPreview, "preview", previewData{Title: "Trim " + plan.Name + " · dry run (nothing removed)", Text: trimText(plan)}), nil
		}
		if len(plan.Remove) == 0 {
			p.info("trim " + plan.Name + ": the rule removes nothing now")
			return s, nil
		}
		p.Status = ""
		return openModal(s, modalConfirm, "confirm", confirmData{Title: "Trim · dry run", Text: plan.Prompt(),
			Action: core.Command{ID: "culprit.trim.apply", Args: map[string]any{"name": plan.Name}}}), nil
	})
	h("culprit.trim.apply", func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		if !confirmed(c) {
			p.warn("trimming needs the confirm: T shows the dry run first")
			return s, nil
		}
		cu, err := app.Cfg.Resolve(c.Arg("name"))
		if err != nil {
			p.fail(err.Error())
			return s, nil
		}
		env := app.Env
		p.info("trimming " + cu.Name + "…")
		return s, []core.Effect{core.AsyncEffect{Slot: "apply", Command: "apply.done", Run: func(ctx context.Context) (any, error) {
			res, err := Trim(ctx, env, cu)
			if err != nil {
				return nil, err
			}
			return fmt.Sprintf("trimmed %s: removed %d files, freed %s", res.Name, len(res.Remove), Human(res.Freed)), nil
		}}}
	})

	// --- relocate
	h("culprit.relocate", func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		id, ok := p.focusedName()
		if !ok {
			p.warn("no culprit under the cursor")
			return s, nil
		}
		specs := []core.ParamSpec{
			{Name: "to", Type: core.ParamString, Required: true, Description: "destination root; each path keeps its place under home"},
			{Name: "link", Type: core.ParamBool, Description: "leave a symlink at each old path"},
			{Name: "same_volume", Type: core.ParamBool, Description: "allow a destination on the same volume (frees nothing)"},
		}
		m := form.New("Relocate "+id, specs, map[string]any{"to": app.relocateTo(""), "link": false, "same_volume": false})
		m.SubmitLabel = "Dry run"
		m.Editing = true
		return openModal(s, modalForm, "form", relocateForm{Name: id, M: m}), nil
	})
	for _, id := range append([]string{form.CmdInsert, form.CmdBackspace, form.CmdToggle, form.CmdSubmit, form.CmdCancel}, input.ReadlineCommandIDs()...) {
		scoped("form", id, u.formCmd)
	}
	h("culprit.relocate.apply", func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		if !confirmed(c) {
			p.warn("relocating needs the confirm: m shows the dry run first")
			return s, nil
		}
		cu, err := app.Cfg.Resolve(c.Arg("name"))
		if err != nil {
			p.fail(err.Error())
			return s, nil
		}
		o := RelocateOptions{To: c.Arg("to")}
		o.Link, _ = c.Args["link"].(bool)
		o.SameVolume, _ = c.Args["same_volume"].(bool)
		env := app.Env
		p.info("relocating " + cu.Name + "…")
		return s, []core.Effect{core.AsyncEffect{Slot: "apply", Command: "apply.done", Run: func(ctx context.Context) (any, error) {
			res, err := Relocate(ctx, env, cu, o)
			if err != nil {
				return nil, err
			}
			return "relocated " + res.Name + " to " + env.Tilde(res.Dir) + ", freed " + Human(res.Freed), nil
		}}}
	})

	h("apply.done", func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		if err, _ := c.Args["error"].(error); err != nil {
			p.fail("error: " + err.Error())
		} else {
			msg, _ := c.Args["value"].(string)
			p.note(msg)
		}
		p.Grid.ClearMarks()
		return s, []core.Effect{u.scanEffect(p)}
	})

	// --- modals
	h("confirm.yes", func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		d, ok := modalData(s, modalConfirm).(confirmData)
		if !ok {
			return s, nil
		}
		s = closeModal(s, modalConfirm)
		act := d.Action
		args := maps.Clone(act.Args)
		if args == nil {
			args = map[string]any{}
		}
		args["confirmed"] = true
		act.Args, act.Source, act.Actor = args, "confirm", core.Actor{Kind: "user"}
		return s, []core.Effect{dispatch(act)}
	})
	h("confirm.no", func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		p.info("cancelled — nothing changed")
		return closeModal(s, modalConfirm), nil
	})
	h("preview.close", func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		return closeModal(s, modalPreview), nil
	})
	h("palette.open", u.paletteOpen)
	for _, id := range append([]string{"palette.insert", "palette.backspace", "palette.close", picker.CmdConfirm, picker.CmdModeCycle}, input.ReadlineCommandIDs()...) {
		scoped("palette", id, u.paletteCmd)
	}
	h("help.open", func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		return openModal(s, modalHelp, "help", 0), nil
	})
	h("help.close", func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		return closeModal(s, modalHelp), nil
	})
	h("app.close", func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		return s, []core.Effect{core.QuitEffect{}}
	})
	return e
}

// scanEffect sizes every culprit off the loop.
func (u *UI) scanEffect(p *Page) core.Effect {
	p.Loading = true
	app, all := u.App, p.All
	return core.AsyncEffect{Slot: "scan", Command: "scan.done", Run: func(ctx context.Context) (any, error) {
		res := app.scan(ctx, all)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return scanDone{Reports: res.Culprits, Disk: res.Disk}, nil
	}}
}

func sortLabel(p *Page) string {
	dir := "ascending"
	if p.Desc {
		dir = "descending"
	}
	return "sorted by " + p.Sort + " " + dir
}

// trimText lists a trim plan: each file it would remove and why.
func trimText(r TrimResult) string {
	var b strings.Builder
	if len(r.Remove) == 0 {
		fmt.Fprintf(&b, "The rule removes nothing now; it keeps %d files.\n", r.Kept)
	} else {
		fmt.Fprintf(&b, "Would remove %d files, %s; keeps %d.\n\n", len(r.Remove), Human(r.Bytes), r.Kept)
		for _, it := range r.Remove {
			fmt.Fprintf(&b, "  %7s  %s  (%s)\n", Human(it.Bytes), it.Path, it.Reason)
		}
	}
	b.WriteString("\nT trims (asks first) · esc closes")
	return b.String()
}

func (u *UI) scrollModal(id string) fn {
	return func(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
		page := max(s.Height*70/100-4, 1)
		n := c.Times()
		delta := map[string]int{"motion.down": n, "motion.up": -n, "motion.page.down": n * page, "motion.page.up": -n * page,
			"motion.half.down": n * page / 2, "motion.half.up": -n * page / 2, "motion.top": -1 << 30, "motion.bottom": 1 << 30}[c.ID]
		clampOff := func(off, total int) int { return min(max(off, 0), max(total-page, 0)) }
		switch d := modalData(s, id).(type) {
		case int:
			return setModalData(s, id, clampOff(d+delta, len(u.helpLines()))), nil
		case confirmData:
			d.Off = clampOff(d.Off+delta, strings.Count(d.Text, "\n")+1)
			return setModalData(s, id, d), nil
		case previewData:
			d.Off = clampOff(d.Off+delta, strings.Count(d.Text, "\n")+1)
			return setModalData(s, id, d), nil
		}
		return s, nil
	}
}

// --- palette ---------------------------------------------------------------------------

func (u *UI) paletteOpen(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
	keys := keysByCommand(u, "normal")
	var items []picker.Item
	for _, spec := range u.Specs {
		if spec.HasTag("user") {
			items = append(items, paletteItem(spec, keys))
		}
	}
	for _, spec := range core.MotionSpecs() {
		if !slices.Contains(unusedMotions, spec.ID) {
			items = append(items, paletteItem(spec, keys))
		}
	}
	m := picker.New("Commands", items)
	m.Scope, m.Placeholder, m.MatchMode = "palette", "type to filter commands · tab cycles match mode", picker.MatchMode(p.Match)
	return openModal(s, modalPalette, "palette", m), nil
}

func paletteItem(spec core.CommandSpec, keys map[string][]string) picker.Item {
	sub := spec.Description
	if k := keys[spec.ID]; len(k) > 0 {
		sub += "  [" + strings.Join(k, ", ") + "]"
	}
	return picker.Item{ID: spec.ID, Title: spec.ID, Subtitle: sub, Keywords: []string{spec.Description}}
}

func (u *UI) paletteCmd(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
	old, ok := modalData(s, modalPalette).(*picker.Model)
	if !ok {
		return s, nil
	}
	m := *old
	switch c.ID {
	case "palette.insert":
		m.AppendQuery(c.Arg("text"))
	case "palette.backspace":
		m.BackspaceQuery()
	case "palette.close":
		return closeModal(s, modalPalette), nil
	default:
		m.PageSize = max(s.Height*70/100-4, 1)
		picked, _ := m.HandleCommand(c)
		if picked != nil {
			s = closeModal(s, modalPalette)
			return s, []core.Effect{dispatch(core.Command{ID: picked.ID, Source: "palette", Actor: core.Actor{Kind: "user"}})}
		}
	}
	if m.FilterErr != "" {
		p.warn("regex: " + m.FilterErr)
	}
	return setModalData(s, modalPalette, &m), nil
}

// --- relocate form -------------------------------------------------------------------

func (u *UI) formCmd(s core.State, p *Page, c core.Command) (core.State, []core.Effect) {
	rf, ok := modalData(s, modalForm).(relocateForm)
	if !ok {
		return s, nil
	}
	m := *rf.M
	m.Values = maps.Clone(rf.M.Values)
	m.Errors = maps.Clone(rf.M.Errors)
	rf.M = &m
	// Space on a yes/no field flips it; elsewhere it is typed.
	if c.ID == form.CmdInsert && c.Arg("text") == " " && m.KindAt(m.ActiveIdx) == form.KindBool {
		c = core.Command{ID: form.CmdToggle}
	}
	res, _ := m.HandleCommand(c)
	if c.ID == "motion.down" || c.ID == "motion.up" {
		m.Editing = m.Editable()
		m.HandleCommand(core.Command{ID: form.CmdEdit})
	}
	switch {
	case res.Cancelled:
		p.info("relocate cancelled — nothing changed")
		return closeModal(s, modalForm), nil
	case res.Submitted:
		to := strings.TrimSpace(fmt.Sprint(res.Values["to"]))
		if to == "" || to == "<nil>" {
			p.fail(u.App.noDestination().Error())
			return setModalData(s, modalForm, rf), nil
		}
		cu, err := u.App.Cfg.Resolve(rf.Name)
		if err != nil {
			p.fail(err.Error())
			return closeModal(s, modalForm), nil
		}
		link, _ := res.Values["link"].(bool)
		same, _ := res.Values["same_volume"].(bool)
		text := namedPrompt(relocateConfirm, cu.Name, cu) + "\n\n"
		r := p.byName[cu.Name]
		var bd strings.Builder
		bd.WriteString(text)
		for _, f := range r.Found {
			if f.State == "present" {
				fmt.Fprintf(&bd, "  %7s  %s → %s/%s\n", Human(f.Usage.Bytes), u.App.Env.Tilde(f.Path), strings.TrimRight(to, "/"), relUnder(u.App.Env, f.Path))
			}
		}
		if link {
			bd.WriteString("\nA symlink is left at each old path.")
		}
		if same {
			bd.WriteString("\nSame volume allowed: frees nothing.")
		}
		s = closeModal(s, modalForm)
		return openModal(s, modalConfirm, "confirm", confirmData{Title: "Relocate · dry run", Text: strings.TrimRight(bd.String(), "\n"),
			Action: core.Command{ID: "culprit.relocate.apply", Args: map[string]any{"name": cu.Name, "to": to, "link": link, "same_volume": same}}}), nil
	case len(res.ErrorFields) > 0:
		var msgs []string
		for _, name := range res.ErrorFields {
			msgs = append(msgs, name+": "+m.Errors[name])
		}
		p.fail("not submitted — " + strings.Join(msgs, "; "))
	}
	return setModalData(s, modalForm, rf), nil
}

// --- shell -------------------------------------------------------------------------------

// newShell builds the engine and tea shell; the returned command starts
// the first sizing.
func newShell(u *UI) (*otea.Shell, tea.Cmd) {
	p := Page{Match: u.Match, Ratio: u.Ratio, Detail: u.Detail, Density: u.Density, All: u.All, Sort: "size", Desc: true}
	p.Grid = *table.New(gridColumns, nil)
	if len(u.Notes) > 0 {
		p.warn(strings.Join(u.Notes, " · "))
	}
	initial := core.NewState(core.Frame{ID: pageID, FocusID: nodeGrid, Mode: "normal", Data: p})
	sh := otea.New(newEngine(u), initial, u.view, u.theme(&p))
	sh.OnPassThrough = func(e core.PassToWidgetEffect) tea.Cmd {
		if e.Mouse == nil || e.NodeID != nodeGrid {
			return nil
		}
		id := map[string]string{"wheeldown": "motion.down", "wheelup": "motion.up"}[e.Mouse.Button]
		if id == "" {
			return nil
		}
		return func() tea.Msg { return core.CommandEvent{Command: core.Command{ID: id, Count: 3, Source: "mouse"}} }
	}
	sh.OnCustom = func(e core.CustomEffect) tea.Cmd {
		switch d := e.Data.(type) {
		case core.Command:
			if e.Kind == "dispatch" {
				return func() tea.Msg { return core.CommandEvent{Command: d} }
			}
		case ui.Theme:
			sh.Theme = d
		}
		return nil
	}
	_, cmd := sh.Update(core.CommandEvent{Command: core.Command{ID: "culprit.refresh", Source: "startup", Actor: core.Actor{Kind: "system"}}})
	return sh, cmd
}

// startup runs the first sizing from Init.
type startup struct {
	*otea.Shell
	cmd tea.Cmd
}

func (s startup) Init() tea.Cmd { return s.cmd }

func runTUI(a *App, cfg UIConfig) error {
	sh, cmd := newShell(newUI(a, cfg))
	_, err := tea.NewProgram(startup{sh, cmd}).Run()
	if errors.Is(err, tea.ErrProgramKilled) {
		return nil
	}
	return err
}
