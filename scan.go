package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

// scan and top read and change nothing. The answer is the value (JSON);
// --format table gives the table, and is the default when a person is at
// the terminal (Env.TTY).

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

// scan sizes every culprit (only the present ones unless all) and reads
// the disk's totals.
func (a *App) scan(ctx context.Context, all bool) ScanResult {
	reps := ProbeAll(ctx, a.Env, a.Cfg.Culprits)
	if !all {
		reps = presentOnly(reps)
	}
	disk, _ := DiskOf(a.Env.Home)
	res := ScanResult{Disk: disk, Culprits: reps}
	for _, r := range reps {
		res.Total += r.Bytes
	}
	return res
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

// TopOptions are top's flags.
type TopOptions struct {
	Path     string
	Depth, N int
	Min      string
	Timeout  time.Duration
	External bool
}

// top walks PATH once and lists the biggest directories and files.
func (a *App) top(ctx context.Context, o TopOptions) (TopResult, error) {
	if o.Path == "" {
		o.Path = "."
	}
	if o.Min == "" {
		o.Min = "100M"
	}
	if o.Timeout <= 0 {
		o.Timeout = 5 * time.Minute
	}
	matches, literal := a.Env.Expand(o.Path)
	if len(matches) > 0 {
		literal = matches[0]
	}
	if resolved, err := filepath.EvalSymlinks(literal); err == nil {
		literal = resolved
	}
	if _, elsewhere := a.Env.Elsewhere(literal); elsewhere && !(o.External || a.Env.External) {
		return TopResult{}, fmt.Errorf("%s is on another volume than the home directory; --external (or \"external\": true in the culprits file) reads it", literal)
	}
	min, err := ParseSize(o.Min)
	if err != nil {
		return TopResult{}, err
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	entries, total, partial, err := Top(ctx, literal, o.Depth, min)
	if err != nil {
		return TopResult{}, err
	}
	kept := entries[:0]
	for _, e := range entries {
		if e.Usage.Bytes >= min {
			kept = append(kept, e)
		}
	}
	if o.N > 0 && len(kept) > o.N {
		kept = kept[:o.N]
	}
	return TopResult{Root: literal, Total: total, Entries: kept, Seconds: time.Since(start).Seconds(), Partial: partial}, nil
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
