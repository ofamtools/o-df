// Command o-df finds and clears the usual disk hogs. Each culprit (a
// cache, a store, a download folder) is a data row: the embedded defaults
// plus ~/.config/o-df/culprits.json.
//
//	o-df [scan] [--all] [--json]                 culprits by size, and the disk's totals
//	o-df top [PATH] [--depth N] [--n N] [--min SIZE] [--json]
//	o-df free|delete [NAME...] [--all] [--yes]   culprit clear for several names, or every rebuildable one
//	o-df culprit list|show NAME|size NAME|clear NAME|relocate NAME [--to DIR] [--link] [--yes]
//	o-df tui [--smoke]                           the culprits page
//	o-df catalog
//
// Every NOUN VERB command prints JSON (--format table for a table).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	ocli "ofam.dev/ochazuke/cli"
	"ofam.dev/oden"
)

func main() {
	os.Exit(oden.Report(os.Stderr, progName(), Main(os.Args[1:], DefaultEnv(), os.Stdin, os.Stdout, os.Stderr)))
}

func progName() string {
	if parent := os.Getenv("ODEN_PARENT"); parent != "" {
		return parent + " df"
	}
	return "o-df"
}

// Main runs one invocation.
func Main(args []string, env Env, stdin io.Reader, stdout, stderr io.Writer) error {
	a, err := New(env)
	if err != nil {
		return err
	}
	a.Stdin, a.Stdout, a.Stderr = stdin, stdout, stderr
	app := oden.New("o-df", args, oden.WithBuiltins(), oden.WithStdio(stdin, stdout, stderr), oden.WithCategories(categories),
		oden.WithDefault(func(_ context.Context, args []string) error {
			if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
				return oden.ErrNotFound
			}
			return scan(a, args)
		}))
	sh := a.Shell()
	app.Add(oden.Command{Name: "scan", Summary: "the culprits by size and the disk's totals (o-df with no command too)", Usage: "[--all] [--json] [--timeout 60s] [--external]", Source: "o-df",
		Run: func(_ context.Context, args []string) error { return scan(a, args) }})
	app.Add(oden.Command{Name: "top", Summary: "the biggest directories and files under PATH (default .): one parallel walk, this volume only", Usage: "[PATH] [--depth N] [--n N] [--min SIZE] [--timeout 5m] [--external] [--json]", Source: "o-df",
		Run: func(_ context.Context, args []string) error { return top(a, args) }})
	app.Add(oden.Command{Name: "tui", Summary: "open the culprits page: the list, the focused culprit's detail; space marks, d d clears, D clears all, m relocates", Usage: "[--smoke] [--all] [--width N] [--height N]", Source: "o-df",
		Run: func(_ context.Context, args []string) error { return runTUI(a, args) }, Aliases: []string{"view"}})
	// free and delete are culprit clear (its free and delete aliases) at
	// the top level, taking several names: one handler, one confirm.
	app.Add(oden.Command{Name: "free", Aliases: []string{"delete"}, Summary: "clear culprits by NAME..., or --all: every present rebuildable one (culprit clear; one confirm)", Usage: "[NAME...] [--all] [--yes] [--format F]", Source: "o-df",
		Run: func(ctx context.Context, args []string) error {
			return sh.Run(ctx, append([]string{"culprit", "clear"}, freeArgs(args)...))
		}})
	// The tool's own commands are added first: Mount reads a word that names
	// no command through the nouns' verbs, so a command wins over a verb of
	// the same name.
	ocli.Mount(app, sh)
	return app.Run(context.Background())
}

// categories group o-df's own commands first in o-df --help; the
// framework's nouns list under Other.
var categories = map[string]string{"scan": "Disk", "top": "Disk", "free": "Disk", "culprit": "Disk", "tui": "Disk"}

// valueFlags are the culprit clear flags that take the next word as
// their value, so free does not read it as a name.
var valueFlags = map[string]bool{"--format": true, "--target": true, "--json": true, "--name": true, "--names": true}

// freeArgs turns free's words into culprit clear's: one NAME stays the
// positional name, several become --names.
func freeArgs(args []string) []string {
	var names, rest []string
	for i := 0; i < len(args); i++ {
		w := args[i]
		if !strings.HasPrefix(w, "-") {
			names = append(names, w)
			continue
		}
		rest = append(rest, w)
		if valueFlags[w] && i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
			i++
			rest = append(rest, args[i])
		}
	}
	switch len(names) {
	case 0:
		return rest
	case 1:
		return append(names, rest...)
	}
	list, _ := json.Marshal(names)
	return append(rest, "--names", string(list))
}

func flags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return &oden.ExitError{Code: 0}
		}
		return &oden.ExitError{Code: 2}
	}
	return nil
}

// ScanResult is scan --json.
type ScanResult struct {
	Disk     Disk     `json:"disk"`
	Total    int64    `json:"total"`
	Culprits []Report `json:"culprits"`
}

func scan(a *App, args []string) error {
	fs := flags("o-df scan", a.Stderr)
	all := fs.Bool("all", false, "include culprits none of whose paths exist")
	asJSON := fs.Bool("json", false, "print JSON")
	fs.DurationVar(&ProbeTimeout, "timeout", ProbeTimeout, "give up on a path whose walk takes longer")
	fs.BoolVar(&a.Env.External, "external", a.Env.External, "also size paths that resolve onto another volume")
	if err := parse(fs, args); err != nil {
		return err
	}
	ctx := context.Background()
	reps := ProbeAll(ctx, a.Env, a.Cfg.Culprits)
	if !*all {
		reps = presentOnly(reps)
	}
	disk, _ := DiskOf(a.Env.Home)
	res := ScanResult{Disk: disk, Culprits: reps}
	for _, r := range reps {
		res.Total += r.Bytes
	}
	if *asJSON {
		return writeJSON(a.Stdout, res)
	}
	fmt.Fprintln(a.Stdout, diskLine(disk))
	tw := tabwriter.NewWriter(a.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "CULPRIT\tSIZE\tREBUILD\tUSED\tADVICE\tPATH")
	for _, r := range reps {
		fmt.Fprintln(tw, strings.Join(a.cells(r), "\t"))
	}
	fmt.Fprintf(tw, "total\t%s\t\t\t\t%d culprits\n", Human(res.Total), len(reps))
	return tw.Flush()
}

func diskLine(d Disk) string {
	return fmt.Sprintf("%s  size %s  used %s  free %s  %d%%", d.Path, Human(d.Size), Human(d.Used), Human(d.Free), d.Usage)
}

// TopResult is top --json.
type TopResult struct {
	Root    string  `json:"root"`
	Total   Usage   `json:"total"`
	Entries []Entry `json:"entries"`
	Seconds float64 `json:"seconds"`
	// Partial says the walk hit --timeout: the entries are what it had
	// finished, and the total is unknown.
	Partial bool `json:"partial,omitempty"`
}

func top(a *App, args []string) error {
	fs := flags("o-df top", a.Stderr)
	depth := fs.Int("depth", 2, "report directories down to this many levels below PATH")
	n := fs.Int("n", 30, "print this many entries (0: all)")
	minText := fs.String("min", "100M", "report files at least this big (and drop smaller directories)")
	asJSON := fs.Bool("json", false, "print JSON")
	timeout := fs.Duration("timeout", 5*time.Minute, "stop walking after this long and print what was found (marked partial)")
	fs.BoolVar(&a.Env.External, "external", a.Env.External, "allow a PATH on another volume")
	// PATH may come before or after the flags.
	var root string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		root, args = args[0], args[1:]
	}
	if err := parse(fs, args); err != nil {
		return err
	}
	if root == "" {
		root = fs.Arg(0)
	}
	if root == "" {
		root = "."
	}
	matches, literal := a.Env.Expand(root)
	if len(matches) > 0 {
		literal = matches[0]
	}
	if resolved, err := filepath.EvalSymlinks(literal); err == nil {
		literal = resolved
	}
	if _, elsewhere := a.Env.Elsewhere(literal); elsewhere && !a.Env.External {
		return fmt.Errorf("%s is on another volume than the home directory; --external (or \"external\": true in the culprits file) reads it", literal)
	}
	min, err := ParseSize(*minText)
	if err != nil {
		return err
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	entries, total, partial, err := Top(ctx, literal, *depth, min)
	if err != nil {
		return err
	}
	kept := entries[:0]
	for _, e := range entries {
		if e.Usage.Bytes >= min {
			kept = append(kept, e)
		}
	}
	if *n > 0 && len(kept) > *n {
		kept = kept[:*n]
	}
	res := TopResult{Root: literal, Total: total, Entries: kept, Seconds: time.Since(start).Seconds(), Partial: partial}
	if *asJSON {
		return writeJSON(a.Stdout, res)
	}
	if partial {
		fmt.Fprintf(a.Stdout, "%s  partial: stopped after %s; the entries are the directories it finished\n", a.Env.Tilde(literal), *timeout)
	} else {
		fmt.Fprintf(a.Stdout, "%s  %s in %d files  (%.1fs)\n", a.Env.Tilde(literal), Human(total.Bytes), total.Files, res.Seconds)
	}
	tw := tabwriter.NewWriter(a.Stdout, 0, 2, 2, ' ', 0)
	for _, e := range kept {
		kind := "file"
		if e.Dir {
			kind = "dir"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", Human(e.Usage.Bytes), kind, Ago(a.Now(), e.Usage.LastUsed), a.Env.Tilde(e.Path))
	}
	return tw.Flush()
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
