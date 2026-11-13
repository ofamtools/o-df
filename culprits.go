package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// A culprit is one row of data: a named group of paths that grows on its
// own (a cache, a store, a download folder), how to size it, whether it is
// rebuildable (only those can be cleared) and how to clear it. The defaults
// are embedded (culprits.json); the user's file adds rows, replaces a row of
// the same name whole, or drops one with "disabled": true. o-df has no code
// per culprit.

//go:embed culprits.json
var defaultCulprits []byte

// Culprit is one row.
type Culprit struct {
	Name string `json:"name"`
	// Aliases are more words that name the row (brew for homebrew-cache);
	// a name or alias resolves exactly, a prefix of one when it is unique
	// (resolve.go).
	Aliases  []string `json:"aliases,omitempty"`
	Title    string   `json:"title,omitempty"`
	Category string   `json:"category,omitempty"`
	// Paths are ~-relative or absolute; a * matches within one path
	// segment (filepath.Glob). A path holding {project_root} stands for
	// one path per entry of the file's project_roots (none when the list
	// is empty). A missing path is absent, not an error.
	Paths []string `json:"paths"`
	// PathsFrom, when set, is a command (argv) whose stdout lines are the
	// paths, asked each time the row is probed (go env GOCACHE). Its paths are
	// added to Paths, each location listed and sized once; a missing or
	// failing command adds none. See Env.Query.
	PathsFrom []string `json:"paths_from,omitempty"`
	// Rebuildable says the owning tool recreates the data on demand; only
	// a rebuildable culprit can be cleared.
	Rebuildable bool   `json:"rebuildable"`
	Rebuild     string `json:"rebuild,omitempty"`
	// Size, when set, is a command whose stdout's first integer is the
	// culprit's size in bytes; otherwise o-df walks the paths.
	Size []string `json:"size,omitempty"`
	// Clear, when set, is the commands clear runs, each an argv; a command
	// holding {path} runs once per present path. Otherwise clear removes the
	// contents of each path (the directory itself stays).
	Clear [][]string `json:"clear,omitempty"`
	// MinAge, when set ("24h", "7d"), leaves out a matched path whose
	// newest file is younger (state "young"): it is neither sized nor
	// cleared. An open or running thing keeps writing, so it stays young.
	MinAge string `json:"min_age,omitempty"`
	// Skip are base-name globs of matched paths to leave out (state
	// "skipped"), for the live ones (mtm-daemon).
	Skip []string `json:"skip,omitempty"`
	// SkipFrom is a command (argv) whose stdout lines are more Skip globs,
	// asked on each probe (the names of running servers). Without Env.Query,
	// or when it fails, every match is left out: it fails closed.
	SkipFrom []string `json:"skip_from,omitempty"`
	// SkipWith are names that, found within three levels below a matched
	// directory, leave it out (".git": a checkout is never scratch).
	SkipWith []string `json:"skip_with,omitempty"`
	// Owner, when "self", leaves out (state "skipped") a matched path that
	// another user owns: o-df could not remove it, and it is not ours.
	Owner string `json:"owner,omitempty"`
	// SkipHidden leaves out a matched path whose name starts with a dot.
	SkipHidden bool `json:"skip_hidden,omitempty"`
	// Types, when set, are the kinds of matched path to keep (dir, file,
	// symlink, socket, fifo, device); any other kind is left out.
	Types []string `json:"types,omitempty"`
	// RemoveMatches makes clear delete each matched path itself, not just
	// its contents (a glob of scratch directories or staged files).
	RemoveMatches bool `json:"remove_matches,omitempty"`
	// Recommend is the advice the scan prints: delete, relocate or keep.
	Recommend string `json:"recommend,omitempty"`
	Notes     string `json:"notes,omitempty"`
	// External lets this row size a path that resolves onto another
	// volume than the home directory's (skipped by default).
	External bool `json:"external,omitempty"`
	Disabled bool `json:"disabled,omitempty"`
	// Source is where the row came from: "default" or the user file.
	Source string `json:"source,omitempty"`
}

// Config is a culprits file.
type Config struct {
	// RelocateTo is the directory relocate uses when --to is not given.
	// The defaults set none: relocate without either fails, it never
	// guesses a volume.
	RelocateTo string `json:"relocate_to,omitempty"`
	// ProjectRoots are the directories that hold projects (~/src, say);
	// a culprit path holding {project_root} expands once per root. The
	// defaults set none, so per-project rows find nothing until the
	// user's file names their roots.
	ProjectRoots []string `json:"project_roots,omitempty"`
	// External lets every row (and top) read paths on other volumes.
	External bool      `json:"external,omitempty"`
	Culprits []Culprit `json:"culprits"`
}

// Env is where o-df looks: the home directory and the user's culprits file.
type Env struct {
	Home     string
	UserFile string
	// External allows paths that resolve onto another volume than the
	// home directory's (the culprits file's "external", --external).
	External bool
	// TTY says a person is at the terminal: scan and top default to their table.
	TTY bool
	// ProjectRoots fill {project_root} in culprit paths (the culprits
	// file's "project_roots").
	ProjectRoots []string
	// Run runs one clear command (a row's clear argv) and returns its
	// combined output. DefaultEnv sets ExecCommand; a test sets a fake
	// that records the argv and runs nothing. With none set, Clear runs a
	// command only when Home is the real home directory (CommandRunner).
	Run func(ctx context.Context, argv []string) ([]byte, error)
	// Query runs a read-only command for a row's paths_from and returns
	// its stdout. DefaultEnv sets ExecOutput; a test sets a fake. With
	// none set, paths_from is not asked and the row's Paths stand.
	// Tmp is what {tmp} stands for in a culprit path (the system scratch
	// directory); DefaultEnv sets /private/tmp on macOS.
	Tmp string
	// Mine says whether the current user owns a path (the row's owner:
	// "self"); nil reads the owner from the file system. A test sets a fake.
	Mine func(fi fs.FileInfo) bool
	// Now is the clock for min_age; nil is time.Now.
	Now   func() time.Time
	Query func(ctx context.Context, argv []string) ([]byte, error)
}

// ProjectRootPlaceholder in a culprit path stands for each project root.
const ProjectRootPlaceholder = "{project_root}"

// DefaultEnv reads HOME and the config directory ($O_DF_CULPRITS, else
// $XDG_CONFIG_HOME/o-df/culprits.json, else ~/.config/o-df/culprits.json).
func DefaultEnv() Env {
	home, _ := os.UserHomeDir()
	file := os.Getenv("O_DF_CULPRITS")
	if file == "" {
		dir := os.Getenv("XDG_CONFIG_HOME")
		if dir == "" {
			dir = filepath.Join(home, ".config")
		}
		file = filepath.Join(dir, "o-df", "culprits.json")
	}
	return Env{Home: home, UserFile: file, Run: ExecCommand, Query: ExecOutput, Tmp: defaultTmp(), TTY: stdoutIsTerminal()}
}

// Load merges the embedded defaults with the user's file (absent is fine)
// and returns the enabled rows in name order.
func Load(env Env) (Config, error) {
	var base Config
	if err := decode(defaultCulprits, &base); err != nil {
		return Config{}, fmt.Errorf("embedded culprits.json: %w", err)
	}
	for i := range base.Culprits {
		base.Culprits[i].Source = "default"
	}
	if env.UserFile != "" {
		data, err := os.ReadFile(env.UserFile)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return Config{}, err
		default:
			var user Config
			if err := decode(data, &user); err != nil {
				return Config{}, fmt.Errorf("%s: %w", env.UserFile, err)
			}
			base = merge(base, user, env.UserFile)
		}
	}
	out := base.Culprits[:0]
	for _, c := range base.Culprits {
		if !c.Disabled {
			out = append(out, c)
		}
	}
	base.Culprits = out
	sort.Slice(base.Culprits, func(i, j int) bool { return base.Culprits[i].Name < base.Culprits[j].Name })
	return base, validate(base)
}

func decode(data []byte, into *Config) error {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	return dec.Decode(into)
}

func merge(base, user Config, source string) Config {
	if user.RelocateTo != "" {
		base.RelocateTo = user.RelocateTo
	}
	if user.ProjectRoots != nil {
		base.ProjectRoots = user.ProjectRoots
	}
	base.External = base.External || user.External
	for _, c := range user.Culprits {
		c.Source = source
		if i := slices.IndexFunc(base.Culprits, func(b Culprit) bool { return b.Name == c.Name }); i >= 0 {
			base.Culprits[i] = c
		} else {
			base.Culprits = append(base.Culprits, c)
		}
	}
	return base
}

// validateMatchRules checks a row's owner and types.
func validateMatchRules(c Culprit) error {
	if c.Owner != "" && c.Owner != "self" {
		return fmt.Errorf("culprit %q: owner %q is not \"self\"", c.Name, c.Owner)
	}
	for _, t := range c.Types {
		if !slices.Contains(pathTypes, t) {
			return fmt.Errorf("culprit %q: type %q is not one of %s", c.Name, t, strings.Join(pathTypes, ", "))
		}
	}
	return nil
}

func validate(cfg Config) error {
	for _, root := range cfg.ProjectRoots {
		if strings.TrimSpace(root) == "" || strings.Contains(root, ProjectRootPlaceholder) {
			return fmt.Errorf("project_roots: %q is not a directory", root)
		}
	}
	seen := map[string]bool{}
	for _, c := range cfg.Culprits {
		switch {
		case c.Name == "" || strings.ContainsAny(c.Name, "/ "):
			return fmt.Errorf("culprit %q: a name is one word without / or spaces", c.Name)
		case seen[c.Name]:
			return fmt.Errorf("culprit %q is listed twice", c.Name)
		case len(c.Paths) == 0 && len(c.Size) == 0 && len(c.PathsFrom) == 0:
			return fmt.Errorf("culprit %q has no paths", c.Name)
		}
		for _, alias := range c.Aliases {
			if alias == "" || strings.ContainsAny(alias, "/ ") {
				return fmt.Errorf("culprit %q: alias %q is not one word without / or spaces", c.Name, alias)
			}
		}
		if c.MinAge != "" {
			if _, err := ParseAge(c.MinAge); err != nil {
				return fmt.Errorf("culprit %q: min_age: %w", c.Name, err)
			}
		}
		if err := validateMatchRules(c); err != nil {
			return err
		}
		if c.SkipFrom != nil && len(c.SkipFrom) == 0 {
			return fmt.Errorf("culprit %q: an empty skip_from command", c.Name)
		}
		if c.PathsFrom != nil && len(c.PathsFrom) == 0 {
			return fmt.Errorf("culprit %q: an empty paths_from command", c.Name)
		}
		for _, cmd := range c.Clear {
			if len(cmd) == 0 {
				return fmt.Errorf("culprit %q: an empty clear command", c.Name)
			}
		}
		seen[c.Name] = true
	}
	return nil
}

// Templates are a culprit path with {project_root} filled in once per
// project root (no paths when there are none), else the path alone.
func (env Env) Templates(p string) []string {
	if env.Tmp != "" {
		p = strings.ReplaceAll(p, TmpPlaceholder, strings.TrimRight(env.Tmp, "/"))
	}
	if !strings.Contains(p, ProjectRootPlaceholder) {
		return []string{p}
	}
	out := make([]string, 0, len(env.ProjectRoots))
	for _, root := range env.ProjectRoots {
		out = append(out, strings.ReplaceAll(p, ProjectRootPlaceholder, strings.TrimRight(root, "/")))
	}
	return out
}

// Expand is a path with ~ and $HOME made absolute, its globs matched; the
// paths that exist, in order, plus the literal path when nothing matched
// (so an absent culprit still shows where it would be).
func (env Env) Expand(p string) (present []string, literal string) {
	p = os.Expand(p, func(k string) string {
		if k == "HOME" {
			return env.Home
		}
		return os.Getenv(k)
	})
	if p == "~" {
		p = env.Home
	} else if rest, ok := strings.CutPrefix(p, "~/"); ok {
		p = filepath.Join(env.Home, rest)
	}
	matches, _ := filepath.Glob(p)
	return matches, p
}

// Tilde shortens a path under home to ~/...
func (env Env) Tilde(p string) string {
	if env.Home == "" {
		return p
	}
	if p == env.Home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(p, env.Home+string(filepath.Separator)); ok {
		return "~/" + rest
	}
	return p
}

// TmpPlaceholder in a culprit path stands for Env.Tmp.
const TmpPlaceholder = "{tmp}"

func defaultTmp() string {
	if _, err := os.Stat("/private/tmp"); err == nil {
		return "/private/tmp"
	}
	return os.TempDir()
}

// ParseAge reads "90m", "24h" or "7d".
func ParseAge(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		var days int
		if _, err := fmt.Sscanf(n, "%d", &days); err != nil || days < 0 {
			return 0, fmt.Errorf("%q is not an age", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%q is not an age", s)
	}
	return d, nil
}

// stdoutIsTerminal reports whether standard output is a terminal.
func stdoutIsTerminal() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
