// Command o-df finds and clears the usual disk hogs. Each culprit (a
// cache, a store, a download folder) is a data row: the embedded defaults
// plus ~/.config/o-df/culprits.json.
//
//	o-df [scan] [--all] [--format table]         culprit scan: culprits by size, and the disk's totals
//	o-df top [PATH] [--depth N] [--n N] [--min SIZE]   culprit top: the biggest directories and files
//	o-df free|delete [NAME...] [--all] [--yes]   culprit clear for several names, or every rebuildable one
//	o-df culprit list|show NAME|size NAME|clear NAME|relocate NAME [--to DIR] [--link] [--yes]
//	o-df tui [--smoke]                           the culprits page
//	o-df catalog
//
// Every NOUN VERB command prints JSON (--format table for a table).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"strings"

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
	sh := a.Shell()
	// A table answer replaces the JSON the shell wrote (culprit.scan and
	// culprit.top with format table): the shell writes to buf, the table
	// goes out in its place.
	var buf bytes.Buffer
	sh.Stdout = &buf
	app := oden.New("o-df", args, oden.WithBuiltins(), oden.WithStdio(stdin, stdout, stderr), oden.WithCategories(categories),
		oden.WithDefault(func(ctx context.Context, args []string) error {
			if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
				return oden.ErrNotFound
			}
			return sh.Run(ctx, append([]string{"culprit", "scan"}, args...))
		}))
	app.Add(oden.Command{Name: "tui", Summary: "open the culprits page: the list, the focused culprit's detail; space marks, d d clears, D clears all, m relocates", Usage: "[--smoke] [--all] [--width N] [--height N]", Source: "o-df",
		Run: func(_ context.Context, args []string) error { return runTUI(a, args) }, Aliases: []string{"view"}})
	// The tool's own commands are added first: Mount reads a word that names
	// no command through the nouns' verbs, so a command wins over a verb of
	// the same name.
	ocli.Mount(app, sh)
	err = app.Run(context.Background())
	if a.text != nil {
		_, werr := io.WriteString(stdout, *a.text+"\n")
		if err == nil {
			err = werr
		}
		return err
	}
	if _, werr := stdout.Write(buf.Bytes()); err == nil {
		err = werr
	}
	return err
}

// categories group o-df's own commands first in o-df --help; the
// framework's nouns list under Other.
var categories = map[string]string{"culprit": "Disk", "tui": "Disk"}

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

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
