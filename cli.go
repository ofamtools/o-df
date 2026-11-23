package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	ochacli "ofam.dev/ochazuke/cli"
	"ofam.dev/ochazuke/config"
	"ofam.dev/ochazuke/core"
	"ofam.dev/ochazuke/ui"
	"ofam.dev/oden"
)

// The command line. Every read command prints JSON (--format table, or a
// person at the terminal, gets a table); every command that changes
// something asks on stdin first (--yes skips the question) and prints
// what it did as JSON.
//
//	o-df [scan] [--json] [--all]       the culprits page at a terminal, else the scan as JSON
//	o-df top [PATH] [--depth N] [--n N] [--min SIZE]
//	o-df culprits                      every culprit row, unsized
//	o-df show NAME | size NAME
//	o-df clean NAME... | --all [--yes] (aliases clear, free, delete)
//	o-df trim NAME [--apply] [--yes]   a dry run unless --apply
//	o-df relocate NAME [--to DIR] [--link] [--same-volume] [--yes]
//	o-df tui [--all] [--smoke]         the culprits page
//	o-df catalog | query [TEXT]        the catalog protocol (read-only)
//	o-df culprit VERB ...              the v1 noun-verb words, kept
//	o-df --smoke | --version | --help

const usage = `usage: o-df [COMMAND] [ARGS]

Disk:
  scan [--all] [--external] [--timeout D] [--json|--format json|table]
                         the culprits by size and the disk's totals; at a terminal
                         with no format, o-df and o-df scan open the culprits page
  top [PATH] [--depth N] [--n N] [--min SIZE] [--timeout D] [--external] [--format F]
                         the biggest directories and files under PATH (default .)
  culprits [--format F]  every culprit row of the merged culprits file (unsized)
  show NAME              one culprit's row and every path it expands to, sized
  size NAME              one culprit's size
  clean NAME... | --all [--names JSON] [--yes]
                         empty rebuildable culprits (asks first; clear, free, delete)
  trim NAME [--apply] [--yes]
                         a row's retention rule: a dry run unless --apply (asks first)
  relocate NAME [--to DIR] [--link] [--same-volume] [--yes]
                         copy, verify, delete the source (asks first)
  tui [--all] [--smoke [--width N --height N]]
                         the culprits page (--smoke renders one frame)

Catalog protocol (read-only):
  catalog                the manifest: the df.culprit schema and o-df's commands
  query [TEXT] [--kind df.culprit] [--unsized] [--present] [--timeout D]
                         {"items": [...], "total": n} of sized culprit records

Other:
  culprit list|show|size|get|scan|top|clear|free|delete|clear-all|trim|relocate ...
                         the noun-verb words of o-df v1, kept
  --smoke                a scripted session over temp fixtures (keys, engine, renderer)
  --version, --help

A culprit name may be an alias or a unique prefix. Culprits:
$O_DF_CULPRITS, else $XDG_CONFIG_HOME/o-df/culprits.json, else ~/.config/o-df/culprits.json.
`

// exitError carries an exit code; Err, when set, is printed.
type exitError struct {
	Code int
	Err  error
}

func (e *exitError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit %d", e.Code)
	}
	return e.Err.Error()
}

func (e *exitError) Unwrap() error { return e.Err }

func usageErr(format string, a ...any) error {
	return &exitError{Code: 2, Err: fmt.Errorf(format, a...)}
}

// interactiveTerminal says stdin and stdout are a terminal (the page can run).
var interactiveTerminal = func() bool {
	for _, f := range []*os.File{os.Stdin, os.Stdout} {
		fi, err := f.Stat()
		if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
			return false
		}
	}
	return true
}

// Main runs one invocation through the uniform Ochazuke v2 / Oden v2 CLI shell.
func Main(args []string, env Env, stdin io.Reader, stdout, stderr io.Writer) error {
	app := newOdenApp(args, env, stdin, stdout, stderr)
	return app.Run(context.Background())
}

func newOdenApp(args []string, env Env, stdin io.Reader, stdout, stderr io.Writer) *oden.App {
	var cached *cli
	getCLI := func() (*cli, error) {
		if cached != nil {
			return cached, nil
		}
		a, err := New(env)
		if err != nil {
			return nil, err
		}
		a.Stdin, a.Stdout, a.Stderr = stdin, stdout, stderr
		cached = &cli{a: a, in: bufio.NewReader(stdin)}
		return cached, nil
	}
	withCLI := func(fn func(c *cli, args []string) error) func(*oden.Ctx) error {
		return func(oc *oden.Ctx) error {
			c, err := getCLI()
			if err != nil {
				return err
			}
			return fn(c, oc.Args)
		}
	}

	info := ochacli.AppInfo{
		Name:     "o-df",
		Version:  Version,
		Summary:  "Find and clear the usual disk hogs (caches, stores, downloads)",
		HelpText: usage,
		Modes:    func() []core.Mode { return modes },
		Bindings: func() []core.Binding { return defaultBindings() },
		Specs:    func() []core.CommandSpec { return commandSpecs() },
		RunTUI: func(_ context.Context, rootArgs []string) error {
			c, err := getCLI()
			if err != nil {
				return err
			}
			return c.scan(rootArgs, true)
		},
		RunSmoke: func(out io.Writer, _ bool) error {
			if runSmoke(out) != 0 {
				return &exitError{Code: 1, Err: errors.New("smoke failed")}
			}
			return nil
		},
		Manifest: dfExtManifest(),
		Capabilities: &oden.Capabilities{
			Name:         "o-df",
			Version:      Version,
			Summary:      "Find and clear the usual disk hogs (caches, stores, downloads)",
			Capabilities: []string{"query", "catalog", "tui"},
		},
		Query: func(ctx context.Context, kind string, qargs []string) (oden.ExtQueryResult, error) {
			c, err := getCLI()
			if err != nil {
				return oden.ExtQueryResult{}, err
			}
			return c.queryExt(ctx, kind, qargs)
		},
	}

	app := ochacli.NewApp(info, args, oden.WithStdio(stdin, stdout, stderr))

	app.Add(
		oden.Command{
			Name:    "scan",
			Summary: "size culprits and disk totals (opens TUI at a terminal)",
			Usage:   "[--all] [--external] [--timeout D] [--json|--format json|table]",
			Handle:  withCLI(func(c *cli, a []string) error { return c.scan(a, true) }),
		},
		oden.Command{
			Name:    "top",
			Summary: "biggest directories and files under PATH",
			Usage:   "[PATH] [--depth N] [--n N] [--min SIZE] [--timeout D] [--external] [--format F]",
			Handle:  withCLI(func(c *cli, a []string) error { return c.top(a) }),
		},
		oden.Command{
			Name:    "culprits",
			Summary: "list every culprit row of the merged culprits file (unsized)",
			Usage:   "[--json|--format json|table]",
			Handle:  withCLI(func(c *cli, a []string) error { return c.culprits(a) }),
		},
		oden.Command{
			Name:    "list",
			Summary: "list sized culprit rows, biggest first",
			Usage:   "[--all] [--external] [--json|--format json|table]",
			Handle:  withCLI(func(c *cli, a []string) error { return c.list(a) }),
		},
		oden.Command{
			Name:    "show",
			Summary: "show one culprit's row and sized paths",
			Usage:   "NAME [--json|--format json|table]",
			Handle:  withCLI(func(c *cli, a []string) error { return c.show(a) }),
		},
		oden.Command{
			Name:    "size",
			Summary: "print one culprit's size as JSON",
			Usage:   "NAME",
			Handle:  withCLI(func(c *cli, a []string) error { return c.size(a) }),
		},
		oden.Command{
			Name:    "clean",
			Aliases: []string{"clear", "free", "delete"},
			Summary: "empty rebuildable culprits (asks first unless --yes)",
			Usage:   "NAME... | --all [--names JSON] [--yes]",
			Handle:  withCLI(func(c *cli, a []string) error { return c.clean(a, false) }),
		},
		oden.Command{
			Name:    "trim",
			Summary: "apply a culprit's retention rule (dry run unless --apply)",
			Usage:   "NAME [--apply] [--yes]",
			Handle:  withCLI(func(c *cli, a []string) error { return c.trim(a, false) }),
		},
		oden.Command{
			Name:    "relocate",
			Summary: "copy, verify, and delete a culprit's source paths",
			Usage:   "NAME [--to DIR] [--link] [--same-volume] [--yes]",
			Handle:  withCLI(func(c *cli, a []string) error { return c.relocate(a) }),
		},
		oden.Command{
			Name:    "tui",
			Aliases: []string{"view"},
			Summary: "open the interactive culprits page",
			Usage:   "[--all] [--smoke [--width N --height N]]",
			Handle:  withCLI(func(c *cli, a []string) error { return c.tui(a) }),
		},
		oden.Command{
			Name:    "culprit",
			Summary: "v1 noun-verb culprit commands",
			Usage:   "<verb> [ARGS]",
			Children: []oden.Command{
				{Name: "list", Summary: "list sized culprit rows", Handle: withCLI(func(c *cli, a []string) error { return c.list(a) })},
				{Name: "names", Summary: "list matching culprit names", Handle: withCLI(func(c *cli, a []string) error { return c.names(a) })},
				{Name: "show", Summary: "show one culprit's details", Handle: withCLI(func(c *cli, a []string) error { return c.show(a) })},
				{Name: "size", Summary: "show one culprit's size", Handle: withCLI(func(c *cli, a []string) error { return c.size(a) })},
				{Name: "get", Summary: "get a culprit row or property", Handle: withCLI(func(c *cli, a []string) error { return c.get(a) })},
				{Name: "scan", Summary: "scan culprits without opening TUI", Handle: withCLI(func(c *cli, a []string) error { return c.scan(a, false) })},
				{Name: "top", Summary: "biggest directories and files", Handle: withCLI(func(c *cli, a []string) error { return c.top(a) })},
				{Name: "clear", Aliases: []string{"free", "delete"}, Summary: "empty rebuildable culprits", Handle: withCLI(func(c *cli, a []string) error { return c.clean(a, false) })},
				{Name: "clear-all", Summary: "empty all rebuildable culprits", Handle: withCLI(func(c *cli, a []string) error { return c.clean(a, true) })},
				{Name: "trim", Summary: "trim a culprit by retention rule", Handle: withCLI(func(c *cli, a []string) error { return c.trim(a, true) })},
				{Name: "relocate", Summary: "relocate a culprit", Handle: withCLI(func(c *cli, a []string) error { return c.relocate(a) })},
			},
		},
	)

	// Allow opening a directory/file path directly (delegates to top) while keeping
	// oden's default handling for empty args, flags, and unknown commands.
	app.Default = func(ctx context.Context, rootArgs []string) error {
		if len(rootArgs) == 0 || strings.HasPrefix(rootArgs[0], "-") {
			return info.RunTUI(ctx, rootArgs)
		}
		if _, err := os.Stat(rootArgs[0]); err == nil {
			c, err := getCLI()
			if err != nil {
				return err
			}
			return c.top(rootArgs)
		}
		return oden.ErrNotFound
	}

	return app
}

func dfExtManifest() *oden.ExtManifest {
	m := catalogManifest()
	schemas := make([]oden.ExtSchema, 0, len(m.Schemas))
	for _, s := range m.Schemas {
		fields := make([]oden.ExtField, 0, len(s.Fields))
		for _, f := range s.Fields {
			fields = append(fields, oden.ExtField{
				Name:        f.Name,
				Type:        string(f.Type),
				Required:    f.Required,
				Description: f.Description,
				Enum:        f.Enum,
				Default:     f.Default,
				RefKinds:    f.RefKinds,
			})
		}
		schemas = append(schemas, oden.ExtSchema{
			Kind:           s.Kind,
			Title:          s.Title,
			Plural:         s.Plural,
			Fields:         fields,
			DefaultColumns: s.DefaultColumns,
			SummaryField:   s.SummaryField,
			UniqueFields:   s.UniqueFields,
		})
	}
	cmds := make([]oden.ExtCommand, 0, len(m.Commands))
	for _, cmd := range m.Commands {
		params := make([]oden.ExtField, 0, len(cmd.Params))
		for _, p := range cmd.Params {
			params = append(params, oden.ExtField{
				Name:        p.Name,
				Type:        string(p.Type),
				Required:    p.Required,
				Description: p.Description,
				Enum:        p.Enum,
				Default:     p.Default,
				RefKinds:    p.RefKinds,
			})
		}
		cmds = append(cmds, oden.ExtCommand{
			ID:          cmd.ID,
			Description: cmd.Description,
			Params:      params,
			Tags:        cmd.Tags,
			Meta:        cmd.Meta,
		})
	}
	return &oden.ExtManifest{
		Name:     m.Name,
		Version:  m.Version,
		Schemas:  schemas,
		Commands: cmds,
	}
}

// QueryTimeout bounds disk sizing during `o-df query` so catalog hosts never hang.
var QueryTimeout = 10 * time.Second

func (c *cli) queryExt(ctx context.Context, kind string, args []string) (oden.ExtQueryResult, error) {
	if kind != "" && kind != CulpritKind && kind != "culprit" {
		return oden.ExtQueryResult{}, fmt.Errorf("unknown kind %q (expected %s)", kind, CulpritKind)
	}
	p, err := parseArgs(args, []string{"sized", "unsized", "all", "present", "external"}, []string{"format", "timeout"})
	if err != nil {
		return oden.ExtQueryResult{}, err
	}
	if p.has["sized"] && p.has["unsized"] {
		return oden.ExtQueryResult{}, usageErr("query takes --sized or --unsized, not both")
	}
	q := strings.Join(p.pos, " ")
	var cs []Culprit
	for _, cu := range c.a.Cfg.Culprits {
		if matchesQuery(cu, q) {
			cs = append(cs, cu)
		}
	}
	res := oden.ExtQueryResult{Items: []oden.ExtRecord{}}
	toExt := func(rec Record) oden.ExtRecord {
		return oden.ExtRecord{
			Kind:            rec.Kind,
			ID:              rec.ID,
			Fields:          rec.Fields,
			ArchiveStatus:   rec.ArchiveStatus,
			StarStatus:      rec.StarStatus,
			ChangeSetNumber: rec.ChangeSetNumber,
			CreatedAt:       rec.CreatedAt,
			UpdatedAt:       rec.UpdatedAt,
		}
	}
	if p.has["unsized"] {
		for _, cu := range cs {
			res.Items = append(res.Items, toExt(culpritRecord(cu, nil)))
		}
	} else {
		timeout := QueryTimeout
		if d := p.str("timeout", ""); d != "" {
			t, err := time.ParseDuration(d)
			if err != nil {
				return oden.ExtQueryResult{}, usageErr("--timeout: %v", err)
			}
			timeout = t
		}
		env := c.a.Env
		if p.has["external"] {
			env.External = true
		}
		qctx := ctx
		var cancel context.CancelFunc
		if timeout > 0 {
			qctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		reps := ProbeAll(qctx, env, cs)
		if p.has["present"] {
			reps = presentOnly(reps)
		}
		for _, r := range reps {
			res.Items = append(res.Items, toExt(culpritRecord(r.Culprit, &r)))
		}
	}
	res.Total = len(res.Items)
	return res, nil
}

type cli struct {
	a  *App
	in *bufio.Reader
}

// --- arguments -------------------------------------------------------------------------

type parsed struct {
	pos  []string
	vals map[string]string
	has  map[string]bool
}

func (p parsed) str(name, def string) string {
	if v, ok := p.vals[name]; ok {
		return v
	}
	return def
}

func (p parsed) int(name string, def int) (int, error) {
	v, ok := p.vals[name]
	if !ok {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, usageErr("--%s %q: not a number", name, v)
	}
	return n, nil
}

// parseArgs reads flags anywhere among the arguments: --flag, -flag,
// --flag=value, --flag value; "--" ends the flags. bools take no value.
func parseArgs(args []string, bools, values []string) (parsed, error) {
	p := parsed{vals: map[string]string{}, has: map[string]bool{}}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			p.pos = append(p.pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			p.pos = append(p.pos, arg)
			continue
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if name == "y" {
			name = "yes"
		}
		switch {
		case slices.Contains(bools, name):
			if hasVal && (val == "false" || val == "0") {
				continue
			}
			p.has[name] = true
		case slices.Contains(values, name):
			if !hasVal {
				if i+1 >= len(args) {
					return p, usageErr("--%s needs a value", name)
				}
				i++
				val = args[i]
			}
			p.vals[name] = val
			p.has[name] = true
		default:
			return p, usageErr("unknown flag %s", arg)
		}
	}
	return p, nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// asTable says the answer is a table: --format table, or no format at a terminal.
func (c *cli) asTable(p parsed) (bool, error) {
	switch f := p.str("format", ""); f {
	case "":
		return c.a.Env.TTY && !p.has["json"], nil
	case "table":
		return true, nil
	case "json":
		return false, nil
	default:
		return false, usageErr("--format %q: use json or table", f)
	}
}

func (c *cli) out(v any, table bool, text func() string) error {
	if table {
		_, err := io.WriteString(c.a.Stdout, text()+"\n")
		return err
	}
	return writeJSON(c.a.Stdout, v)
}

// confirm asks prompt on stderr and reads the answer from stdin; yes skips it.
func (c *cli) confirm(prompt string, yes bool) error {
	if yes {
		return nil
	}
	fmt.Fprintf(c.a.Stderr, "%s [y/N] ", prompt)
	line, _ := c.in.ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	fmt.Fprintln(c.a.Stderr)
	return &exitError{Code: 1, Err: errors.New("cancelled: nothing changed")}
}

func one(p parsed, verb string) (string, error) {
	pos := p.pos
	if n := strings.TrimSpace(p.str("name", "")); n != "" {
		pos = append(slices.Clone(pos), n)
	}
	switch len(pos) {
	case 0:
		return "", usageErr("a culprit is required: o-df %s NAME (o-df culprits names them)", verb)
	case 1:
		return pos[0], nil
	}
	return "", usageErr("o-df %s takes one culprit (got %s)", verb, strings.Join(pos, " "))
}

// --- reads -------------------------------------------------------------------------------

func (c *cli) scan(args []string, page bool) error {
	p, err := parseArgs(args, []string{"all", "external", "json"}, []string{"timeout", "format"})
	if err != nil {
		return err
	}
	if len(p.pos) > 0 {
		return usageErr("scan takes no arguments (got %s)", strings.Join(p.pos, " "))
	}
	if p.has["external"] {
		c.a.Env.External = true
	}
	if d := p.str("timeout", ""); d != "" {
		t, err := time.ParseDuration(d)
		if err != nil {
			return usageErr("--timeout: %v", err)
		}
		ProbeTimeout = t
	}
	if page && c.a.Env.TTY && !p.has["json"] && p.str("format", "") == "" && interactiveTerminal() {
		return runTUI(c.a, c.uiConfig(p.has["all"]))
	}
	table, err := c.asTable(p)
	if err != nil {
		return err
	}
	res := c.a.scan(context.Background(), p.has["all"])
	return c.out(res, table, func() string { return c.a.scanText(res) })
}

func (c *cli) top(args []string) error {
	p, err := parseArgs(args, []string{"external", "json"}, []string{"path", "depth", "n", "min", "timeout", "format"})
	if err != nil {
		return err
	}
	o := TopOptions{Path: p.str("path", "."), Min: p.str("min", "100M"), External: p.has["external"], Timeout: 5 * time.Minute}
	if len(p.pos) > 1 {
		return usageErr("top takes one PATH (got %s)", strings.Join(p.pos, " "))
	} else if len(p.pos) == 1 {
		o.Path = p.pos[0]
	}
	if o.Depth, err = p.int("depth", 2); err != nil {
		return err
	}
	if o.N, err = p.int("n", 30); err != nil {
		return err
	}
	if d := p.str("timeout", ""); d != "" {
		if o.Timeout, err = time.ParseDuration(d); err != nil {
			return usageErr("--timeout: %v", err)
		}
	}
	table, err := c.asTable(p)
	if err != nil {
		return err
	}
	res, err := c.a.top(context.Background(), o)
	if err != nil {
		return err
	}
	return c.out(res, table, func() string { return c.a.topText(res, o.Timeout) })
}

func (c *cli) culprits(args []string) error {
	p, err := parseArgs(args, []string{"json"}, []string{"format"})
	if err != nil {
		return err
	}
	table, err := c.asTable(p)
	if err != nil {
		return err
	}
	return c.out(c.a.Cfg.Culprits, table, func() string {
		var b strings.Builder
		tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "CULPRIT\tCATEGORY\tREBUILD\tADVICE\tTRIM\tSOURCE")
		for _, cu := range c.a.Cfg.Culprits {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", cu.Name, cu.Category, yesNo(cu.Rebuildable), cu.Recommend, yesNo(cu.Trimmable()), c.a.Env.Tilde(cu.Source))
		}
		tw.Flush()
		return strings.TrimRight(b.String(), "\n")
	})
}

// list is the sized rows: {noun, id, label, data, cells}, biggest first.
func (c *cli) list(args []string) error {
	p, err := parseArgs(args, []string{"all", "external", "json"}, []string{"format"})
	if err != nil {
		return err
	}
	if p.has["external"] {
		c.a.Env.External = true
	}
	table, err := c.asTable(p)
	if err != nil {
		return err
	}
	res := c.a.scan(context.Background(), p.has["all"])
	type row struct {
		Noun  string   `json:"noun"`
		ID    string   `json:"id"`
		Label string   `json:"label"`
		Data  Report   `json:"data"`
		Cells []string `json:"cells"`
	}
	rows := make([]row, len(res.Culprits))
	for i, r := range res.Culprits {
		rows[i] = row{Noun: "culprit", ID: "culprit/" + r.Name, Label: r.Name, Data: r, Cells: c.a.cells(r)}
	}
	return c.out(rows, table, func() string {
		var b strings.Builder
		tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "CULPRIT\tSIZE\tREBUILD\tUSED\tADVICE\tPATH")
		for _, r := range rows {
			fmt.Fprintln(tw, strings.Join(r.Cells, "\t"))
		}
		tw.Flush()
		return strings.TrimRight(b.String(), "\n")
	})
}

func (c *cli) names(args []string) error {
	word := ""
	if len(args) > 0 {
		word = args[0]
	}
	type choice struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	}
	out := []choice{}
	for _, cu := range c.a.Cfg.Candidates(word) {
		out = append(out, choice{ID: cu.Name, Label: cu.Title})
	}
	return writeJSON(c.a.Stdout, out)
}

func (c *cli) show(args []string) error {
	p, err := parseArgs(args, []string{"json"}, []string{"name", "format"})
	if err != nil {
		return err
	}
	name, err := one(p, "show")
	if err != nil {
		return err
	}
	cu, err := c.a.Cfg.Resolve(name)
	if err != nil {
		return err
	}
	table, err := c.asTable(p)
	if err != nil {
		return err
	}
	r := Probe(context.Background(), c.a.Env, cu)
	return c.out(r, table, func() string { return describe(c.a.Env, r, c.a.Now()) })
}

func (c *cli) size(args []string) error {
	p, err := parseArgs(args, nil, []string{"name", "format"})
	if err != nil {
		return err
	}
	name, err := one(p, "size")
	if err != nil {
		return err
	}
	cu, err := c.a.Cfg.Resolve(name)
	if err != nil {
		return err
	}
	return writeJSON(c.a.Stdout, sizeOf(Probe(context.Background(), c.a.Env, cu)))
}

// get is culprit get NAME [--prop size]: the row, or its computed size.
func (c *cli) get(args []string) error {
	p, err := parseArgs(args, nil, []string{"name", "prop", "format"})
	if err != nil {
		return err
	}
	name, err := one(p, "culprit get")
	if err != nil {
		return err
	}
	cu, err := c.a.Cfg.Resolve(name)
	if err != nil {
		return err
	}
	switch prop := p.str("prop", ""); prop {
	case "":
		return writeJSON(c.a.Stdout, cu)
	case "size":
		return writeJSON(c.a.Stdout, sizeOf(Probe(context.Background(), c.a.Env, cu)))
	default:
		rec := culpritRecord(cu, nil)
		v, ok := rec.Fields[prop]
		if !ok {
			return usageErr("culprit has no prop %q", prop)
		}
		return writeJSON(c.a.Stdout, v)
	}
}

// --- changes -------------------------------------------------------------------------------

func (c *cli) clean(args []string, all bool) error {
	p, err := parseArgs(args, []string{"all", "yes"}, []string{"name", "names", "more"})
	if err != nil {
		return err
	}
	all = all || p.has["all"]
	names := cleanNames(p)
	if all && len(names) > 0 {
		return usageErr("clean takes culprit names or --all, not both")
	}
	ctx := context.Background()
	if all || len(names) > 1 {
		plan, err := c.a.planClear(ctx, names, all, nil)
		switch {
		case err != nil:
			return err
		case len(plan.Clear) == 0:
			return errors.New("nothing to clear: no rebuildable culprit is present")
		}
		if err := c.confirm(plan.Prompt(), p.has["yes"]); err != nil {
			return err
		}
		return writeJSON(c.a.Stdout, c.a.clearMany(ctx, plan))
	}
	if len(names) == 0 {
		return usageErr("a culprit is required: o-df clean NAME, or --all (o-df culprits names them)")
	}
	cu, err := c.a.Cfg.Resolve(names[0])
	if err != nil {
		return err
	}
	if !cu.Rebuildable {
		return notRebuildable(cu)
	}
	if err := c.confirm(namedPrompt(clearConfirm, names[0], cu), p.has["yes"]); err != nil {
		return err
	}
	res, err := Clear(ctx, c.a.Env, cu)
	if err != nil {
		return err
	}
	return writeJSON(c.a.Stdout, res)
}

// cleanNames are the positional names, then --name, --more and --names (a JSON
// array or a comma/space list), once each.
func cleanNames(p parsed) []string {
	var names []string
	add := func(n string) {
		if n = strings.TrimSpace(n); n != "" && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	for _, n := range p.pos {
		add(n)
	}
	add(p.str("name", ""))
	for _, n := range strings.Fields(p.str("more", "")) {
		add(n)
	}
	if v := p.str("names", ""); v != "" {
		var list []string
		if err := json.Unmarshal([]byte(v), &list); err != nil {
			list = strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' })
		}
		for _, n := range list {
			add(n)
		}
	}
	return names
}

// trim is a dry run unless --apply; legacy (culprit trim) applies unless
// --dry-run, as o-df v1 did. Applying asks first, listing each file.
func (c *cli) trim(args []string, legacy bool) error {
	p, err := parseArgs(args, []string{"dry-run", "apply", "yes"}, []string{"name"})
	if err != nil {
		return err
	}
	verb := "trim"
	if legacy {
		verb = "culprit trim"
	}
	name, err := one(p, verb)
	if err != nil {
		return err
	}
	cu, err := c.a.Cfg.Resolve(name)
	if err != nil {
		return err
	}
	dry := !p.has["apply"]
	if legacy {
		dry = p.has["dry-run"]
	}
	if p.has["dry-run"] && p.has["apply"] {
		return usageErr("trim takes --dry-run or --apply, not both")
	}
	ctx := context.Background()
	plan, err := PlanTrim(ctx, c.a.Env, cu)
	if err != nil {
		return err
	}
	if dry {
		return writeJSON(c.a.Stdout, plan)
	}
	if len(plan.Remove) > 0 {
		if err := c.confirm(plan.Prompt(), p.has["yes"]); err != nil {
			return err
		}
	}
	res, err := Trim(ctx, c.a.Env, cu)
	if err != nil {
		return err
	}
	return writeJSON(c.a.Stdout, res)
}

func (c *cli) relocate(args []string) error {
	p, err := parseArgs(args, []string{"link", "same-volume", "yes"}, []string{"name", "to"})
	if err != nil {
		return err
	}
	o := RelocateOptions{To: c.a.relocateTo(p.str("to", "")), Link: p.has["link"], SameVolume: p.has["same-volume"]}
	if o.To == "" {
		return c.a.noDestination()
	}
	name, err := one(p, "relocate")
	if err != nil {
		return err
	}
	cu, err := c.a.Cfg.Resolve(name)
	if err != nil {
		return err
	}
	if err := c.confirm(namedPrompt(relocateConfirm, name, cu), p.has["yes"]); err != nil {
		return err
	}
	res, err := Relocate(context.Background(), c.a.Env, cu, o)
	if err != nil {
		return err
	}
	return writeJSON(c.a.Stdout, res)
}

// --- the page ------------------------------------------------------------------------------

func (c *cli) tui(args []string) error {
	p, err := parseArgs(args, []string{"all", "smoke"}, []string{"width", "height"})
	if err != nil {
		return err
	}
	cfg := c.uiConfig(p.has["all"])
	if p.has["smoke"] {
		w, err := p.int("width", 120)
		if err != nil {
			return err
		}
		h, err := p.int("height", 30)
		if err != nil {
			return err
		}
		return frameOnce(c.a, cfg, w, h, c.a.Stdout)
	}
	return runTUI(c.a, cfg)
}

// uiConfig reads the optional bindings.json, themes.json and settings.json
// (JSONC) beside the culprits file. Missing files are fine; broken ones
// are reported as notes and skipped. Nothing is ever written.
func (c *cli) uiConfig(all bool) UIConfig {
	cfg := UIConfig{All: all}
	dir := ""
	if c.a.Env.UserFile != "" {
		dir = filepath.Dir(c.a.Env.UserFile)
	}
	read := func(name string) []byte {
		if dir == "" {
			return nil
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			cfg.Notes = append(cfg.Notes, name+": "+err.Error())
		}
		return data
	}
	if data := read("bindings.json"); data != nil {
		if bs, err := config.LoadBindings(data); err != nil {
			cfg.Notes = append(cfg.Notes, "bindings.json: "+err.Error())
		} else {
			cfg.Bindings = bs
		}
	}
	if data := read("themes.json"); data != nil {
		if th, err := config.LoadTheme(data, ui.DefaultDarkTheme()); err != nil {
			cfg.Notes = append(cfg.Notes, "themes.json: "+err.Error())
		} else {
			cfg.Theme = &th
		}
	}
	set, err := config.LoadSettings(read("settings.json"), defaultSettings)
	if err != nil {
		cfg.Notes = append(cfg.Notes, "settings.json: "+err.Error())
		set, _ = config.LoadSettings(nil, defaultSettings)
	}
	cfg.Settings = set
	return cfg
}
