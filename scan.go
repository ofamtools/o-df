package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"ofam.dev/ochazuke"
)

// scan and top are verbs of the culprit noun (culprit.scan, culprit.top):
// what the top-level words scan and top read. The answer is the value
// (JSON); format table gives the table, and is the default when a person
// is at the terminal (Env.TTY). Neither changes anything.

// ScanResult is culprit.scan's value.
type ScanResult struct {
	Disk     Disk     `json:"disk"`
	Total    int64    `json:"total"`
	Culprits []Report `json:"culprits"`
}

// TopResult is culprit.top's value.
type TopResult struct {
	Root    string  `json:"root"`
	Total   Usage   `json:"total"`
	Entries []Entry `json:"entries"`
	Seconds float64 `json:"seconds"`
	// Partial says the walk hit the timeout: the entries are what it had
	// finished, and the total is unknown.
	Partial bool `json:"partial,omitempty"`
}

// asTable says the request wants the table: format table, or no format at
// a terminal.
func (a *App) asTable(c ochazuke.Ctx) bool {
	switch f := ochazuke.StringArg(c.Args, "format", ""); f {
	case "":
		return a.Env.TTY
	default:
		return f == "table"
	}
}

func (a *App) culpritScan(c ochazuke.Ctx) ochazuke.Result {
	all, _ := c.Args["all"].(bool)
	env := a.Env
	if ext, _ := c.Args["external"].(bool); ext {
		env.External = true
	}
	if d := ochazuke.StringArg(c.Args, "timeout", ""); d != "" {
		t, err := time.ParseDuration(d)
		if err != nil {
			return ochazuke.Failed(fmt.Errorf("timeout: %w", err))
		}
		ProbeTimeout = t
	}
	table := a.asTable(c)
	return ochazuke.Pending(func(ctx context.Context) (any, error) {
		reps := ProbeAll(ctx, env, a.Cfg.Culprits)
		if !all {
			reps = presentOnly(reps)
		}
		disk, _ := DiskOf(env.Home)
		res := ScanResult{Disk: disk, Culprits: reps}
		for _, r := range reps {
			res.Total += r.Bytes
		}
		if table {
			text := a.scanText(res)
			a.text = &text
		}
		return res, nil
	})
}

func (a *App) scanText(res ScanResult) string {
	var b strings.Builder
	fmt.Fprintln(&b, diskLine(res.Disk))
	tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "CULPRIT\tSIZE\tREBUILD\tUSED\tADVICE\tPATH")
	for _, r := range res.Culprits {
		fmt.Fprintln(tw, strings.Join(a.cells(r), "\t"))
	}
	fmt.Fprintf(tw, "total\t%s\t\t\t\t%d culprits\n", Human(res.Total), len(res.Culprits))
	tw.Flush()
	return strings.TrimRight(b.String(), "\n")
}

func diskLine(d Disk) string {
	return fmt.Sprintf("%s  size %s  used %s  free %s  %d%%", d.Path, Human(d.Size), Human(d.Used), Human(d.Free), d.Usage)
}

func (a *App) culpritTop(c ochazuke.Ctx) ochazuke.Result {
	root := ochazuke.StringArg(c.Args, "path", ".")
	depth := 2
	if v, ok := c.Args["depth"]; ok {
		depth = intArg(v, depth)
	}
	n := 30
	if v, ok := c.Args["n"]; ok {
		n = intArg(v, n)
	}
	minText := ochazuke.StringArg(c.Args, "min", "100M")
	timeout := 5 * time.Minute
	if d := ochazuke.StringArg(c.Args, "timeout", ""); d != "" {
		t, err := time.ParseDuration(d)
		if err != nil {
			return ochazuke.Failed(fmt.Errorf("timeout: %w", err))
		}
		timeout = t
	}
	external := a.Env.External
	if ext, _ := c.Args["external"].(bool); ext {
		external = true
	}
	matches, literal := a.Env.Expand(root)
	if len(matches) > 0 {
		literal = matches[0]
	}
	if resolved, err := filepath.EvalSymlinks(literal); err == nil {
		literal = resolved
	}
	if _, elsewhere := a.Env.Elsewhere(literal); elsewhere && !external {
		return ochazuke.Failed(fmt.Errorf("%s is on another volume than the home directory; --external (or \"external\": true in the culprits file) reads it", literal))
	}
	min, err := ParseSize(minText)
	if err != nil {
		return ochazuke.Failed(err)
	}
	table := a.asTable(c)
	return ochazuke.Pending(func(ctx context.Context) (any, error) {
		start := time.Now()
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		entries, total, partial, err := Top(ctx, literal, depth, min)
		if err != nil {
			return nil, err
		}
		kept := entries[:0]
		for _, e := range entries {
			if e.Usage.Bytes >= min {
				kept = append(kept, e)
			}
		}
		if n > 0 && len(kept) > n {
			kept = kept[:n]
		}
		res := TopResult{Root: literal, Total: total, Entries: kept, Seconds: time.Since(start).Seconds(), Partial: partial}
		if table {
			text := a.topText(res, timeout)
			a.text = &text
		}
		return res, nil
	})
}

func (a *App) topText(res TopResult, timeout time.Duration) string {
	var b strings.Builder
	if res.Partial {
		fmt.Fprintf(&b, "%s  partial: stopped after %s; the entries are the directories it finished\n", a.Env.Tilde(res.Root), timeout)
	} else {
		fmt.Fprintf(&b, "%s  %s in %d files  (%.1fs)\n", a.Env.Tilde(res.Root), Human(res.Total.Bytes), res.Total.Files, res.Seconds)
	}
	tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	for _, e := range res.Entries {
		kind := "file"
		if e.Dir {
			kind = "dir"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", Human(e.Usage.Bytes), kind, Ago(a.Now(), e.Usage.LastUsed), a.Env.Tilde(e.Path))
	}
	tw.Flush()
	return strings.TrimRight(b.String(), "\n")
}

func intArg(v any, def int) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case string:
		var n int
		if _, err := fmt.Sscan(x, &n); err == nil {
			return n
		}
	}
	return def
}
