package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"ofam.dev/ochazuke"
	ocli "ofam.dev/ochazuke/cli"
)

// The application: one ochazuke runtime holding the culprit list and the
// culprit noun's verbs, used by the command line (the shell runs one verb
// and settles it) and by the TUI. What o-df read from the disk is the
// list's rows; the runtime holds interaction state only.

const (
	pageID     = "df"
	culpritsID = "df/culprits"
)

// Version is the catalog's version.
const Version = "0.1.0"

var culpritColumns = []ochazuke.Column{
	{Title: "culprit", Min: 12},
	{Title: "size", Align: ochazuke.AlignRight, Priority: 1},
	{Title: "rebuild", Priority: 2},
	{Title: "used", Align: ochazuke.AlignRight, Priority: 3},
	{Title: "advice", Priority: 4},
	{Title: "path", Priority: 5},
}

// App is o-df's state.
type App struct {
	r    *ochazuke.Runtime
	m    *ochazuke.Model
	list *ochazuke.List

	Env Env
	Cfg Config
	Now func() time.Time

	reports []Report
	disk    Disk
	loading string
	note    string // the last change's outcome, shown on the status line
	listErr error
	tui     bool

	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// Namespace is the singular namespace o-df declares (lexicon plan): its
// operations are df.<noun>.<verb> and a host mounts its catalog under it.
const Namespace = "df"

func init() {
	if err := ochazuke.DeclareNamespace(Namespace); err != nil {
		panic(err)
	}
}

// New builds the runtime: the page holding the culprit list, the noun
// with its columns, verbs and bindings, and the model.
func New(env Env) (*App, error) {
	cfg, err := Load(env)
	if err != nil {
		return nil, err
	}
	r := ochazuke.NewRuntime()
	env.External = env.External || cfg.External
	env.ProjectRoots = cfg.ProjectRoots
	a := &App{r: r, Env: env, Cfg: cfg, Now: time.Now, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
	r.Instance(pageID, ochazuke.Kind("df"))
	if a.list, err = r.AddList(culpritsID, nil, ochazuke.Parent(pageID), ochazuke.Columns(culpritColumns...)); err != nil {
		return nil, err
	}
	r.Define("culprit").Columns(culpritColumns...).Props(culpritProps()...).Store(a.culpritStore())
	a.registerVerbs()
	a.m = ochazuke.NewModel(r)
	a.m.Title = "o-df"
	if err := a.bind(); err != nil {
		return nil, err
	}
	// Declared as a data page so a host's catalog can draw the list; the
	// TUI draws it itself (render).
	if err := ocli.DeclarePage(r, ocli.CatalogPage{
		ID: pageID, Title: "Disk", Layout: ocli.LayoutSplit,
		Lists:  []ocli.CatalogList{{ID: culpritsID, Noun: "culprit", Title: "Culprits"}},
		Detail: &ocli.CatalogDetail{Fields: []string{"name", "title", "bytes", "files", "last_used", "rebuildable", "rebuild", "recommend", "notes", "paths", "found"}},
	}); err != nil {
		return nil, err
	}
	page := r.PageOf(pageID)
	page.Render, page.Focus, page.Remembered = a.render, culpritsID, culpritsID
	if err := r.OpenPage(pageID); err != nil {
		return nil, err
	}
	return a, nil
}

// Shell is the NOUN VERB command line over the runtime.
func (a *App) Shell() *ocli.Shell {
	sh := ocli.New(a.r, "o-df", a.Stdin, a.Stdout, a.Stderr)
	sh.Usage = ocli.DefaultUsage("o-df")
	sh.Version = Version
	return sh
}

func (a *App) registerVerbs() {
	r := a.r
	r.Define("culprit").
		Verb("list", a.culpritList, "every culprit with its size, biggest first: rows {noun, id: culprit/NAME, label, data: report, cells}; --all includes the absent ones").Rows("culprit").Fetch().Quiet().
		Verb("names", a.culpritNames, "the culprit names a word may mean, for completion: every name for no query, else the one it resolves to (exact, alias or unique prefix) or the ones an ambiguous prefix shares: [{id: NAME, label: title}]").Fetch().Quiet().
		Verb("show", a.culpritShow, "one culprit's row (paths, rebuildable, clear commands, advice) and every path it expands to, sized").Fetch().Quiet().Positional("name").
		Verb("size", a.culpritSize, "one culprit's size: {name, bytes, human, files, last_used}").Fetch().Quiet().Positional("name").
		Verb("clear", a.culpritClear, "empty rebuildable culprits (their clear commands, else their paths' contents): NAME gives {name, before, after, freed}; --names or --all (every present rebuildable culprit, never one that is not rebuildable) ask once, listing each with its size and the total, and give {cleared, skipped, freed}").Confirm("Clear {{label}}? Its tool rebuilds it on demand.").Positional("name").Alias("free").
		Verb("clear-all", a.culpritClearAll, "empty every present rebuildable culprit after one confirm listing each with its size and the total (clear --all; takes no name, so a key, the help or the palette never mixes it with the focused row): {cleared, skipped, freed}").Confirm("Clear every rebuildable culprit? Their tools rebuild them on demand.").
		Verb("relocate", a.culpritRelocate, "copy a culprit's paths to TO/<path under home>, verify, delete the source; --link leaves a symlink: {name, dir, moved, freed}").Confirm("Relocate {{label}} (copy, verify, delete)?").Positional("name")
	setHistory(r, ochazuke.HistoryChange, "culprit.clear", "culprit.clear-all", "culprit.relocate")
	// A name may be abbreviated (a unique prefix or an alias), so it is
	// Free: the verb resolves it and refuses an ambiguous or unknown one;
	// culprit.names lists what the typed word may mean.
	name := ochazuke.Parameter{Type: "string", Description: "a culprit name, an alias or a unique prefix of one (o-df culprit list; default: the focused row's, which a key, the help and the palette fill)", Source: "culprit.names", Free: true, From: "row:name"}
	set := func(key, result string, params map[string]ochazuke.Parameter) {
		op := r.Operations.At(ochazuke.Key(r.CanonicalKey(key)))
		op.Parameters, op.Result = params, result
		r.Operations.Set(op)
	}
	set("culprit.list", "row[]", map[string]ochazuke.Parameter{
		"all":      {Type: "boolean", Description: "include culprits none of whose paths exist"},
		"external": {Type: "boolean", Description: "also size paths that resolve onto another volume"},
	})
	set("culprit.names", "[{id, label}]", map[string]ochazuke.Parameter{
		ochazuke.ChoicesQueryParameter: {Type: "string", Description: "the word typed so far"},
	})
	set("culprit.show", "report", map[string]ochazuke.Parameter{"name": name})
	set("culprit.size", "{name, bytes, human, files, last_used}", map[string]ochazuke.Parameter{"name": name})
	set("culprit.clear", "{name, before, after, freed} | {cleared, skipped, freed}", map[string]ochazuke.Parameter{
		"name":  name,
		"names": {Type: "array", Description: "several culprit names (each may be a unique prefix or an alias), cleared after one confirm (o-df free NAME NAME... fills it)"},
		"all":   {Type: "boolean", Description: "every present rebuildable culprit, after one confirm listing them; culprits that are not rebuildable (downloads, movies) are never included"},
	})
	set("culprit.clear-all", "{cleared, skipped, freed}", nil)
	set("culprit.relocate", "{name, dir, moved, freed}", map[string]ochazuke.Parameter{
		"name":        name,
		"to":          {Type: "string", Description: "the destination root (default: relocate_to in the culprits file; with neither, relocate fails)"},
		"link":        {Type: "boolean", Description: "leave a symlink at each old path"},
		"same-volume": {Type: "boolean", Description: "allow a destination on the source's volume (frees nothing)"},
	})
	// Checks run before the verbs' confirms, from every path: clearing
	// several culprits asks one question naming each with its size;
	// relocating without a destination fails before asking anything.
	must(r.AddVerbCheck("culprit.clear", ochazuke.VerbCheck{Name: "clear-many", Check: a.checkClear}))
	a.defineCoreDelete()
	setHistory(r, ochazuke.HistoryChange, "culprit.delete")
	must(r.AddVerbCheck("culprit.delete", ochazuke.VerbCheck{Name: "clear-many", Check: a.checkClear}))
	must(r.AddVerbCheck("culprit.clear-all", ochazuke.VerbCheck{Name: "clear-many", Check: func(r *ochazuke.Runtime, cx ochazuke.Context, args ochazuke.Arguments) ochazuke.Verdict {
		return a.checkClear(r, cx, allArgs(args))
	}}))
	must(r.AddVerbCheck("culprit.relocate", ochazuke.VerbCheck{Name: "destination", Check: a.checkRelocate}))
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

// bind installs the key rows: d d clears the marked rows (the focused one
// when none is marked), D clears every present rebuildable culprit (one
// confirm), m relocates the marked rows to relocate_to, R reloads, q
// quits. Space marks (the list noun's row); j/k move.
func (a *App) bind() error {
	r := a.r
	r.Define("df").Mode("normal").
		Bind("R", ochazuke.Do("culprit.list")).
		Bind("D", ochazuke.Do("culprit.clear-all").Hinted("clear all")).
		Bind("q", ochazuke.Do("app.quit"))
	r.Define("culprit").Mode("normal").
		Bind("d d", ochazuke.Do("list.apply", ochazuke.Arguments{"verb": "clear"}).Hinted("clear")).
		Bind("m", ochazuke.Do("list.apply", ochazuke.Arguments{"verb": "relocate"}).Hinted("relocate"))
	return nil
}

// culprit is the one a verb acts on: --name, else the targeted row's.
func (a *App) culprit(c ochazuke.Ctx) (Culprit, error) {
	if n := ochazuke.StringArg(c.Args, "name", ""); n != "" {
		return a.Cfg.Resolve(n)
	}
	if c.Instance != nil {
		if rep, ok := c.Instance.Data.(Report); ok {
			return rep.Culprit, nil
		}
	}
	return Culprit{}, errors.New("a culprit is required: o-df culprit VERB NAME (o-df culprit list names them)")
}

func (a *App) culpritList(c ochazuke.Ctx) ochazuke.Result {
	all, _ := c.Args["all"].(bool)
	if ext, _ := c.Args["external"].(bool); ext {
		a.Env.External = true
	}
	a.loading = "sizing culprits"
	var disk Disk // read with the rows, used by Then (after the work)
	return ochazuke.Pending(func(ctx context.Context) (any, error) {
		reps := ProbeAll(ctx, a.Env, a.Cfg.Culprits)
		if !all {
			reps = presentOnly(reps)
		}
		disk, _ = DiskOf(a.Env.Home)
		return a.rows(reps), ctx.Err()
	}).Replacing("list").Then(func(_ ochazuke.Ctx, v any, err error) {
		a.loading, a.listErr = "", err
		if err != nil {
			return
		}
		rows := v.([]ocli.Row)
		a.disk, a.reports = disk, make([]Report, len(rows))
		items := make([]ochazuke.ListItem, len(rows))
		for i, r := range rows {
			a.reports[i] = r.Data.(Report)
			items[i] = ochazuke.ListItem{ID: r.ID, Label: r.Label, Noun: r.Noun, Data: r.Data, Cells: r.Cells}
		}
		_ = a.list.SetItems(items)
	})
}

func presentOnly(reps []Report) []Report {
	out := reps[:0:0]
	for _, r := range reps {
		if r.Present {
			out = append(out, r)
		}
	}
	return out
}

// culpritNames are the choices for a culprit name (Config.Candidates).
func (a *App) culpritNames(c ochazuke.Ctx) ochazuke.Result {
	cs := a.Cfg.Candidates(ochazuke.StringArg(c.Args, ochazuke.ChoicesQueryParameter, ""))
	out := make([]ochazuke.Choice, len(cs))
	for i, cu := range cs {
		out[i] = ochazuke.Choice{ID: cu.Name, Label: cu.Title}
	}
	return ochazuke.Done(out)
}

func (a *App) culpritShow(c ochazuke.Ctx) ochazuke.Result {
	cu, err := a.culprit(c)
	if err != nil {
		return ochazuke.Failed(err)
	}
	return ochazuke.Pending(func(ctx context.Context) (any, error) { return Probe(ctx, a.Env, cu), nil })
}

// SizeResult is culprit size's answer.
type SizeResult struct {
	Name     string    `json:"name"`
	Bytes    int64     `json:"bytes"`
	Human    string    `json:"human"`
	Files    int64     `json:"files"`
	LastUsed time.Time `json:"last_used,omitzero"`
}

func (a *App) culpritSize(c ochazuke.Ctx) ochazuke.Result {
	cu, err := a.culprit(c)
	if err != nil {
		return ochazuke.Failed(err)
	}
	return ochazuke.Pending(func(ctx context.Context) (any, error) {
		r := Probe(ctx, a.Env, cu)
		return SizeResult{Name: r.Name, Bytes: r.Bytes, Human: Human(r.Bytes), Files: r.Files, LastUsed: r.LastUsed}, nil
	})
}

func (a *App) culpritClear(c ochazuke.Ctx) ochazuke.Result {
	names, all := clearArgs(c.Args)
	if all || len(names) > 1 {
		if all && len(names) > 0 {
			return ochazuke.Failed(errors.New("clear takes culprit names or --all, not both"))
		}
		known := a.known()
		return ochazuke.Pending(func(ctx context.Context) (any, error) {
			plan, err := a.planClear(ctx, names, all, known)
			if err != nil {
				return nil, err
			}
			return a.clearMany(ctx, plan), nil
		}).Then(a.changed(func(v any) string {
			res := v.(ClearManyResult)
			return fmt.Sprintf("cleared %s, freed %s", culprits(len(res.Cleared)), Human(res.Freed))
		}))
	}
	cu, err := a.culprit(c)
	if len(names) == 1 {
		cu, err = a.Cfg.Resolve(names[0])
	}
	if err != nil {
		return ochazuke.Failed(err)
	}
	if !cu.Rebuildable {
		return ochazuke.Failed(fmt.Errorf("%s is not rebuildable; o-df clears only rebuildable culprits (relocate moves it instead)", cu.Name))
	}
	return ochazuke.Pending(func(ctx context.Context) (any, error) {
		return Clear(ctx, a.Env, cu)
	}).Then(a.changed(func(v any) string {
		res := v.(ClearResult)
		return "cleared " + res.Name + ", freed " + Human(res.Freed)
	}))
}

// culpritClearAll is clear --all under its own name.
func (a *App) culpritClearAll(c ochazuke.Ctx) ochazuke.Result {
	c.Args = allArgs(c.Args)
	return a.culpritClear(c)
}

// allArgs is args as clear --all: the all flag only.
func allArgs(_ ochazuke.Arguments) ochazuke.Arguments {
	return ochazuke.Arguments{"all": true}
}

// clearArgs are clear's names (name, then names, once each) and --all.
func clearArgs(args ochazuke.Arguments) (names []string, all bool) {
	all, _ = args["all"].(bool)
	add := func(n string) {
		if n = strings.TrimSpace(n); n != "" && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	add(ochazuke.StringArg(args, "name", ""))
	switch v := args["names"].(type) {
	case []string:
		for _, n := range v {
			add(n)
		}
	case []any:
		for _, n := range v {
			add(fmt.Sprint(n))
		}
	case string:
		for _, n := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' }) {
			add(n)
		}
	}
	return names, all
}

// known is what the TUI last sized (the list's rows), so asking about
// several culprits does not walk them again; nil on the command line,
// where the plan sizes them.
func (a *App) known() []Report {
	if !a.tui || a.reports == nil {
		return nil
	}
	return slices.Clone(a.reports)
}

// ClearPlan is what clearing several culprits will do: the culprits to
// clear with their sizes, and the present ones --all leaves out because
// they are not rebuildable.
type ClearPlan struct {
	Clear    []Report
	Excluded []Report
	Skipped  []string
	Total    int64
}

// planClear sizes the culprits a clear of names (or of all) acts on.
// Every name must resolve to one rebuildable culprit (an ambiguous or
// unknown name fails the whole plan); all takes every present
// rebuildable one and never one that is not rebuildable. known, when
// set, are reports already read (the TUI's rows): a culprit missing
// from them is absent.
func (a *App) planClear(ctx context.Context, names []string, all bool, known []Report) (ClearPlan, error) {
	var cs []Culprit
	if all {
		cs = a.Cfg.Culprits
	} else {
		resolved, err := a.Cfg.ResolveAll(names)
		if err != nil {
			return ClearPlan{}, err
		}
		for _, cu := range resolved {
			if !cu.Rebuildable {
				return ClearPlan{}, fmt.Errorf("%s is not rebuildable; o-df clears only rebuildable culprits (relocate moves it instead)", cu.Name)
			}
			cs = append(cs, cu)
		}
	}
	var reps []Report
	if known != nil {
		for _, cu := range cs {
			i := slices.IndexFunc(known, func(r Report) bool { return r.Name == cu.Name })
			if i >= 0 {
				reps = append(reps, known[i])
			} else {
				reps = append(reps, Report{Culprit: cu})
			}
		}
	} else {
		reps = ProbeAll(ctx, a.Env, cs)
	}
	var plan ClearPlan
	for _, r := range reps {
		switch {
		case !r.Present:
			if !all {
				plan.Skipped = append(plan.Skipped, r.Name+": none of its paths is present")
			}
		case !r.Rebuildable:
			plan.Excluded = append(plan.Excluded, r)
		default:
			plan.Clear = append(plan.Clear, r)
			plan.Total += r.Bytes
		}
	}
	return plan, ctx.Err()
}

// Prompt is the one question clearing several culprits asks.
func (p ClearPlan) Prompt() string {
	var b strings.Builder
	if len(p.Clear) == 1 {
		fmt.Fprintf(&b, "Clear 1 culprit, %s? Its tool rebuilds it on demand.\n", Human(p.Total))
	} else {
		fmt.Fprintf(&b, "Clear %s, %s in all? Their tools rebuild them on demand.\n", culprits(len(p.Clear)), Human(p.Total))
	}
	width := 0
	for _, r := range p.Clear {
		width = max(width, len(r.Name))
	}
	for _, r := range p.Clear {
		fmt.Fprintf(&b, "  %-*s  %6s\n", width, r.Name, Human(r.Bytes))
	}
	for _, s := range p.Skipped {
		fmt.Fprintf(&b, "  skipped %s\n", s)
	}
	if len(p.Excluded) > 0 {
		var ex []string
		for _, r := range p.Excluded {
			ex = append(ex, r.Name+" "+Human(r.Bytes))
		}
		fmt.Fprintf(&b, "Not rebuildable, never included: %s.", strings.Join(ex, ", "))
	} else {
		b.WriteString("Culprits that are not rebuildable (downloads, movies) are never included.")
	}
	return b.String()
}

// culprits is n with the noun: 1 culprit, 3 culprits.
func culprits(n int) string {
	if n == 1 {
		return "1 culprit"
	}
	return fmt.Sprintf("%d culprits", n)
}

// ClearManyResult is what clearing several culprits did: each culprit's
// result, the ones that failed or had nothing present, and the total
// freed.
type ClearManyResult struct {
	Cleared []ClearResult `json:"cleared"`
	Skipped []string      `json:"skipped,omitempty"`
	Freed   int64         `json:"freed"`
}

func (a *App) clearMany(ctx context.Context, plan ClearPlan) ClearManyResult {
	res := ClearManyResult{Cleared: []ClearResult{}, Skipped: plan.Skipped}
	for _, r := range plan.Clear {
		one, err := Clear(ctx, a.Env, r.Culprit)
		if err != nil {
			res.Skipped = append(res.Skipped, r.Name+": "+err.Error())
			if one.Freed == 0 {
				continue
			}
		}
		res.Cleared = append(res.Cleared, one)
		res.Freed += one.Freed
	}
	return res
}

// checkClear asks once before clearing several culprits (--names, --all):
// the prompt names each with its size and the total. One culprit keeps
// the verb's own confirm, asked about its full name when the word was an
// abbreviation (checkName).
func (a *App) checkClear(r *ochazuke.Runtime, _ ochazuke.Context, args ochazuke.Arguments) ochazuke.Verdict {
	names, all := clearArgs(args)
	if all && len(names) > 0 {
		return ochazuke.Refuse("clear takes culprit names or --all, not both")
	}
	if !all && len(names) < 2 {
		if len(names) == 0 {
			return ochazuke.Allow()
		}
		cu, err := a.Cfg.Resolve(names[0])
		if err == nil && !cu.Rebuildable {
			err = fmt.Errorf("%s is not rebuildable; o-df clears only rebuildable culprits (relocate moves it instead)", cu.Name)
		}
		return a.checkName(r, "culprit.clear", names[0], cu, err)
	}
	plan, err := a.planClear(context.Background(), names, all, a.known())
	switch {
	case err != nil:
		return ochazuke.Refuse(err.Error())
	case len(plan.Clear) == 0:
		return ochazuke.Refuse("nothing to clear: no rebuildable culprit is present")
	}
	return ochazuke.AskFirst(plan.Prompt())
}

// checkRelocate refuses a relocate with no destination (no --to, no
// relocate_to) or an ambiguous or unknown name before its confirm.
func (a *App) checkRelocate(r *ochazuke.Runtime, _ ochazuke.Context, args ochazuke.Arguments) ochazuke.Verdict {
	if a.relocateTo(args) == "" {
		return ochazuke.Refuse(a.noDestination().Error())
	}
	if n := ochazuke.StringArg(args, "name", ""); n != "" {
		cu, err := a.Cfg.Resolve(n)
		return a.checkName(r, "culprit.relocate", n, cu, err)
	}
	return ochazuke.Allow()
}

// checkName is a destructive verb's check of the one name it was given:
// refuse what did not resolve; a word that resolved to another name (a
// prefix, an alias) asks the verb's own confirm about the full name, so
// the question says what will be acted on; an exact name keeps the
// verb's confirm as it is.
func (a *App) checkName(r *ochazuke.Runtime, key, word string, cu Culprit, err error) ochazuke.Verdict {
	switch {
	case err != nil:
		return ochazuke.Refuse(err.Error())
	case cu.Name == word:
		return ochazuke.Allow()
	}
	op, _ := r.Operation(key)
	if op.Confirm == "" {
		return ochazuke.Allow()
	}
	return ochazuke.AskFirst(strings.ReplaceAll(op.Confirm, "{{label}}", cu.Name) + fmt.Sprintf(" (%q is %s)", word, cu.Name))
}

// relocateTo is --to, else the culprits file's relocate_to, else "".
func (a *App) relocateTo(args ochazuke.Arguments) string {
	if to := ochazuke.StringArg(args, "to", ""); to != "" {
		return to
	}
	return a.Cfg.RelocateTo
}

func (a *App) noDestination() error {
	return fmt.Errorf("relocate needs a destination and none is set: pass --to DIR, or set \"relocate_to\" in %s", a.Env.UserFile)
}

func (a *App) culpritRelocate(c ochazuke.Ctx) ochazuke.Result {
	cu, err := a.culprit(c)
	if err != nil {
		return ochazuke.Failed(err)
	}
	o := RelocateOptions{To: a.relocateTo(c.Args)}
	if o.To == "" {
		return ochazuke.Failed(a.noDestination())
	}
	o.Link, _ = c.Args["link"].(bool)
	o.SameVolume, _ = c.Args["same-volume"].(bool)
	return ochazuke.Pending(func(ctx context.Context) (any, error) {
		return Relocate(ctx, a.Env, cu, o)
	}).Then(a.changed(func(v any) string {
		res := v.(RelocateResult)
		return "relocated " + res.Name + " to " + a.Env.Tilde(res.Dir) + ", freed " + Human(res.Freed)
	}))
}

// changed reports a change on the TUI and reads the sizes again.
func (a *App) changed(say func(any) string) func(ochazuke.Ctx, any, error) {
	return func(ctx ochazuke.Ctx, v any, err error) {
		if !a.tui {
			return
		}
		if err != nil {
			a.note = "error: " + err.Error()
		} else {
			a.note = say(v)
			ctx.Info(a.note)
		}
		a.refresh()
	}
}

func (a *App) refresh() {
	_, err := a.r.Launch(culpritsID, "culprit.list", a.culpritList(ochazuke.Ctx{Args: ochazuke.Arguments{}}), ochazuke.Ctx{Runtime: a.r, Context: a.r.Context(), Args: ochazuke.Arguments{}, Instance: a.r.Instances[culpritsID]})
	if err != nil {
		a.r.UI().Status(err.Error())
	}
}

// Rows: id culprit/NAME, label NAME, one cell per column.

func (a *App) cells(r Report) []string {
	rb := "no"
	if r.Rebuildable {
		rb = "yes"
	}
	size := Human(r.Bytes)
	if r.Errors > 0 {
		size = "≥" + size
	}
	for _, f := range r.Found {
		if f.Timeout {
			size = "timeout"
		}
	}
	return []string{r.Name, size, rb, Ago(a.Now(), r.LastUsed), r.Recommend, a.pathText(r)}
}

func (a *App) pathText(r Report) string {
	var parts []string
	for _, f := range r.Found {
		switch f.State {
		case "present":
			parts = append(parts, a.Env.Tilde(f.Path))
		case "symlink":
			parts = append(parts, a.Env.Tilde(f.Path)+" → "+f.Target)
		case "external":
			parts = append(parts, a.Env.Tilde(f.Path)+" (other volume, skipped)")
		}
	}
	if len(parts) == 0 && len(r.Found) > 0 {
		return a.Env.Tilde(r.Found[0].Path) + " (absent)"
	}
	return strings.Join(parts, ", ")
}

func (a *App) items(reps []Report) []ochazuke.ListItem {
	out := make([]ochazuke.ListItem, len(reps))
	for i, r := range reps {
		out[i] = ochazuke.ListItem{ID: "culprit/" + r.Name, Label: r.Name, Noun: "culprit", Data: r, Cells: a.cells(r)}
	}
	return out
}

func (a *App) rows(reps []Report) []ocli.Row {
	out := make([]ocli.Row, len(reps))
	for i, it := range a.items(reps) {
		out[i] = ocli.Row{Noun: it.Noun, ID: it.ID, Label: it.Label, Data: it.Data, Cells: it.Cells}
	}
	return out
}

// setHistory declares the history kind of each named verb (from o-git).
func setHistory(r *ochazuke.Runtime, kind string, keys ...string) {
	for _, key := range keys {
		op, ok := r.Operations.Find(ochazuke.Key(r.CanonicalKey(key)))
		if !ok {
			panic("setHistory: no operation " + key)
		}
		op.HistoryKind = kind
		r.Operations.Set(op)
	}
}
