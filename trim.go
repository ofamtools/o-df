package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// A culprit row with keep, days, group and min_keep (culprits.go) can be
// trimmed: its matched files are grouped, and each group keeps its newest
// copies. Trim removes only what the rule says, never the whole culprit, so
// a row that is not rebuildable can carry it. All of it is row data; no code
// knows a folder or a program.

// validateRetention checks a row's retention fields.
func validateRetention(c Culprit) error {
	if c.Keep < 0 || c.Days < 0 || c.MinKeep < 0 {
		return fmt.Errorf("culprit %q: keep, days and min_keep are not negative", c.Name)
	}
	if c.Group != "" {
		re, err := regexp.Compile(c.Group)
		if err != nil {
			return fmt.Errorf("culprit %q: group: %w", c.Name, err)
		}
		if re.NumSubexp() < 1 {
			return fmt.Errorf("culprit %q: group %q needs a capture group naming the group", c.Name, c.Group)
		}
	}
	return nil
}

// Trimmable says the row sets a limit (keep or days).
func (c Culprit) Trimmable() bool { return c.Keep > 0 || c.Days > 0 }

// TrimItem is one file the rule removes (or removed).
type TrimItem struct {
	Path   string `json:"path"`
	Group  string `json:"group,omitempty"`
	Bytes  int64  `json:"bytes"`
	Reason string `json:"reason"`
	Error  string `json:"error,omitempty"`
}

// TrimResult is what trim planned (dry run) or did.
type TrimResult struct {
	Name   string     `json:"name"`
	DryRun bool       `json:"dry_run"`
	Remove []TrimItem `json:"remove"`
	Kept   int        `json:"kept"`
	Bytes  int64      `json:"bytes"`
	Freed  int64      `json:"freed,omitempty"`
}

type trimFile struct {
	path  string
	group string
	bytes int64
	mtime time.Time
}

// PlanTrim lists what the row's rule removes now. It reads and removes
// nothing else.
func PlanTrim(ctx context.Context, env Env, c Culprit) (TrimResult, error) {
	res := TrimResult{Name: c.Name, DryRun: true, Remove: []TrimItem{}}
	if err := validateRetention(c); err != nil {
		return res, err
	}
	if !c.Trimmable() {
		return res, nil
	}
	var re *regexp.Regexp
	if c.Group != "" {
		re = regexp.MustCompile(c.Group)
	}
	groups := map[string][]trimFile{}
	seen := map[string]bool{}
	for _, p := range env.rowPaths(ctx, c) {
		for _, t := range env.Templates(p) {
			matches, _ := env.Expand(t)
			for _, m := range matches {
				m = filepath.Clean(m)
				if seen[m] {
					continue
				}
				seen[m] = true
				group := ""
				if re != nil {
					sub := re.FindStringSubmatch(filepath.Base(m))
					if sub == nil {
						continue
					}
					group = sub[1]
				}
				pu := probePath(ctx, m)
				if pu.State != "present" {
					continue
				}
				mt := pu.Usage.LastUsed
				if mt.IsZero() {
					if fi, err := os.Lstat(m); err == nil {
						mt = fi.ModTime()
					}
				}
				groups[group] = append(groups[group], trimFile{m, group, pu.Usage.Bytes, mt})
			}
		}
	}
	now := env.now()
	limit := time.Duration(c.Days) * 24 * time.Hour
	names := make([]string, 0, len(groups))
	for g := range groups {
		names = append(names, g)
	}
	sort.Strings(names)
	for _, g := range names {
		fs := groups[g]
		// Newest first by modification time; equal times: the later name is newer.
		sort.SliceStable(fs, func(i, j int) bool {
			if !fs[i].mtime.Equal(fs[j].mtime) {
				return fs[i].mtime.After(fs[j].mtime)
			}
			return fs[i].path > fs[j].path
		})
		for i, f := range fs {
			var reason string
			switch {
			case i < c.MinKeep:
			case c.Keep > 0 && i >= c.Keep:
				reason = fmt.Sprintf("beyond the newest %d", c.Keep)
			case c.Days > 0 && now.Sub(f.mtime) > limit:
				reason = fmt.Sprintf("older than %d days", c.Days)
			}
			if reason == "" {
				res.Kept++
				continue
			}
			res.Remove = append(res.Remove, TrimItem{Path: f.path, Group: g, Bytes: f.bytes, Reason: reason})
			res.Bytes += f.bytes
		}
	}
	return res, nil
}

// RemoveFunc is the remover trim uses: Env.Remove, else (only for the real
// home directory) os.RemoveAll.
func (env Env) RemoveFunc() (func(string) error, error) {
	if env.Remove != nil {
		return env.Remove, nil
	}
	real, err := os.UserHomeDir()
	if err != nil || filepath.Clean(real) != filepath.Clean(env.Home) {
		return nil, fmt.Errorf("refusing to remove files: %s is not the home directory and no remover is set (Env.Remove)", env.Home)
	}
	return os.RemoveAll, nil
}

// Trim removes what PlanTrim lists, through Env.Remove; one failing file is
// recorded and never stops the rest.
func Trim(ctx context.Context, env Env, c Culprit) (TrimResult, error) {
	res, err := PlanTrim(ctx, env, c)
	if err != nil {
		return res, err
	}
	res.DryRun = false
	rm, err := env.RemoveFunc()
	if err != nil {
		return res, err
	}
	for i, it := range res.Remove {
		if err := guard(env, it.Path); err != nil {
			res.Remove[i].Error = err.Error()
			continue
		}
		if err := rm(it.Path); err != nil {
			res.Remove[i].Error = err.Error()
			continue
		}
		res.Freed += it.Bytes
	}
	return res, nil
}

// Prompt is the question applying a trim asks.
func (r TrimResult) Prompt() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Trim %s: remove %d files, %s?\n", r.Name, len(r.Remove), Human(r.Bytes))
	for _, it := range r.Remove {
		fmt.Fprintf(&b, "  %s  %s  (%s)\n", Human(it.Bytes), it.Path, it.Reason)
	}
	return strings.TrimRight(b.String(), "\n")
}
