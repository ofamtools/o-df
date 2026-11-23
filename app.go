package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"
)

// The application: what o-df read from the culprits file and the
// environment it acts in, shared by the command line (cli.go) and the
// culprits page (page.go). Every operation here is plain Go over the
// domain (probe, clear, trim, relocate); the confirm wording lives here so
// the command line and the page ask the same question.

// Version is the catalog's version.
const Version = "0.2.0"

// App is o-df's state.
type App struct {
	Env Env
	Cfg Config
	Now func() time.Time

	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// New loads the culprits file for env.
func New(env Env) (*App, error) {
	cfg, err := Load(env)
	if err != nil {
		return nil, err
	}
	env.External = env.External || cfg.External
	env.ProjectRoots = cfg.ProjectRoots
	return &App{Env: env, Cfg: cfg, Now: time.Now, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}, nil
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

// SizeResult is a culprit's size answer.
type SizeResult struct {
	Name     string    `json:"name"`
	Bytes    int64     `json:"bytes"`
	Human    string    `json:"human"`
	Files    int64     `json:"files"`
	LastUsed time.Time `json:"last_used,omitzero"`
}

func sizeOf(r Report) SizeResult {
	return SizeResult{Name: r.Name, Bytes: r.Bytes, Human: Human(r.Bytes), Files: r.Files, LastUsed: r.LastUsed}
}

func notRebuildable(cu Culprit) error {
	return fmt.Errorf("%s is not rebuildable; o-df clears only rebuildable culprits (relocate moves it instead)", cu.Name)
}

// --- confirm wording -------------------------------------------------------------

const (
	clearConfirm    = "Clear {{label}}? Its tool rebuilds it on demand."
	relocateConfirm = "Relocate {{label}} (copy, verify, delete)?"
)

// namedPrompt fills a confirm with the culprit's full name and, when the
// word was an abbreviation (a prefix, an alias), says which word it was.
func namedPrompt(tmpl, word string, cu Culprit) string {
	p := strings.ReplaceAll(tmpl, "{{label}}", cu.Name)
	if word != "" && word != cu.Name {
		p += fmt.Sprintf(" (%q is %s)", word, cu.Name)
	}
	return p
}

// --- clear -----------------------------------------------------------------------

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
// set, are reports already read (the page's rows): a culprit missing
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
				return ClearPlan{}, notRebuildable(cu)
			}
			cs = append(cs, cu)
		}
	}
	var reps []Report
	if known != nil {
		for _, cu := range cs {
			if i := slices.IndexFunc(known, func(r Report) bool { return r.Name == cu.Name }); i >= 0 {
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

// skippedPaths counts the paths clearing could not remove, over every culprit.
func skippedPaths(res ClearManyResult) int {
	n := 0
	for _, c := range res.Cleared {
		n += len(c.Skipped)
	}
	return n
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
		if len(one.Skipped) > 0 {
			res.Skipped = append(res.Skipped, fmt.Sprintf("%s: skipped %d paths: %s", r.Name, len(one.Skipped), strings.Join(one.Skipped, "; ")))
		}
		res.Cleared = append(res.Cleared, one)
		res.Freed += one.Freed
	}
	return res
}

func clearManyNote(res ClearManyResult) string {
	msg := fmt.Sprintf("cleared %s, freed %s", culprits(len(res.Cleared)), Human(res.Freed))
	if n := skippedPaths(res); n > 0 {
		msg += fmt.Sprintf("; skipped %d paths (not yours / permission denied)", n)
	}
	return msg
}

// --- relocate ----------------------------------------------------------------------

// relocateTo is to, else the culprits file's relocate_to, else "".
func (a *App) relocateTo(to string) string {
	if to != "" {
		return to
	}
	return a.Cfg.RelocateTo
}

func (a *App) noDestination() error {
	return fmt.Errorf("relocate needs a destination and none is set: pass --to DIR, or set \"relocate_to\" in %s", a.Env.UserFile)
}

// --- rows -------------------------------------------------------------------------

// cells are a report's table cells: culprit, size, rebuild, used, advice, path.
func (a *App) cells(r Report) []string {
	return []string{r.Name, sizeText(r), yesNo(r.Rebuildable), Ago(a.Now(), r.LastUsed), r.Recommend, a.pathText(r)}
}

func sizeText(r Report) string {
	size := Human(r.Bytes)
	if r.Errors > 0 {
		size = "≥" + size
	}
	for _, f := range r.Found {
		if f.Timeout {
			size = "timeout"
		}
	}
	return size
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
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

// describe is a culprit's detail: its row and every path it expands to.
func describe(env Env, r Report, now time.Time) string {
	var b strings.Builder
	line := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%-11s %s\n", k, v)
		}
	}
	line("culprit", r.Name)
	line("what", r.Title)
	line("category", r.Category)
	line("size", fmt.Sprintf("%s in %d files", Human(r.Bytes), r.Files))
	if r.Errors > 0 {
		line("unreadable", fmt.Sprintf("%d entries (the size is a lower bound)", r.Errors))
	}
	line("size error", r.SizeErr)
	if !r.LastUsed.IsZero() {
		line("last used", r.LastUsed.Format("2006-01-02 15:04")+" ("+Ago(now, r.LastUsed)+" ago)")
	}
	rb := "no: clean refuses; relocate moves it"
	if r.Rebuildable {
		rb = "yes"
	}
	line("rebuildable", rb)
	line("rebuild", r.Rebuild)
	line("advice", r.Recommend)
	if r.Trimmable() {
		rule := []string{}
		if r.Keep > 0 {
			rule = append(rule, fmt.Sprintf("keep %d", r.Keep))
		}
		if r.Days > 0 {
			rule = append(rule, fmt.Sprintf("days %d", r.Days))
		}
		if r.Group != "" {
			rule = append(rule, "group "+r.Group)
		}
		if r.MinKeep > 0 {
			rule = append(rule, fmt.Sprintf("min_keep %d", r.MinKeep))
		}
		line("trim rule", strings.Join(rule, ", "))
	}
	line("notes", r.Notes)
	for _, cmd := range r.Clear {
		line("clear runs", strings.Join(cmd, " "))
	}
	line("source", env.Tilde(r.Source))
	b.WriteString("\npaths\n")
	for _, f := range r.Found {
		switch f.State {
		case "present":
			size := Human(f.Usage.Bytes)
			if f.Timeout {
				size = "timeout"
			}
			fmt.Fprintf(&b, "  %7s  %s\n", size, env.Tilde(f.Path))
		case "symlink":
			fmt.Fprintf(&b, "  %7s  %s → %s\n", "link", env.Tilde(f.Path), f.Target)
		case "external":
			fmt.Fprintf(&b, "  %7s  %s → %s (another volume; external: true reads it)\n", "skipped", env.Tilde(f.Path), f.Target)
		case "absent", "":
			fmt.Fprintf(&b, "  %7s  %s\n", "absent", env.Tilde(f.Path))
		default:
			fmt.Fprintf(&b, "  %7s  %s\n", f.State, env.Tilde(f.Path))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
