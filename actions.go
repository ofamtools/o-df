package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// The two changes: clear (rebuildable rows only) and relocate (copy,
// verify, delete, optionally symlink back). Both act only on the paths a
// culprit lists that are present (not absent, not already a symlink), and
// both refuse a path that is /, the home directory or above it, so a bad
// row in the user's file cannot empty the home.

// ExecCommand runs argv as a process: the real binary's Env.Run.
func ExecCommand(ctx context.Context, argv []string) ([]byte, error) {
	return exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
}

// ExecOutput runs argv and returns its stdout alone: the real binary's
// Env.Query.
func ExecOutput(ctx context.Context, argv []string) ([]byte, error) {
	return exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
}

// CommandRunner is what runs a clear command: Env.Run, else ExecCommand
// when Home is the real home directory. A Home that is not (a test's temp
// HOME) with no runner set is refused: a clear command acts on the
// process's environment (go clean -modcache reads GOMODCACHE), not on
// Home, so it would reach the real caches.
func (env Env) CommandRunner() (func(context.Context, []string) ([]byte, error), error) {
	if env.Run != nil {
		return env.Run, nil
	}
	real, err := os.UserHomeDir()
	if err != nil || filepath.Clean(real) != filepath.Clean(env.Home) {
		return nil, fmt.Errorf("refusing to run a clear command: %s is not the home directory and no command runner is set (Env.Run)", env.Home)
	}
	return ExecCommand, nil
}

// ClearResult is what clear did.
type ClearResult struct {
	Name    string   `json:"name"`
	Before  int64    `json:"before"`
	After   int64    `json:"after"`
	Freed   int64    `json:"freed"`
	Cleared []string `json:"cleared,omitempty"`
	Ran     []string `json:"ran,omitempty"`
	// Skipped are paths that could not be removed (permission denied, not
	// ours); one failing path never stops the rest.
	Skipped []string `json:"skipped,omitempty"`
}

// Clear empties a rebuildable culprit: its clear commands, else the
// contents of each present path.
func Clear(ctx context.Context, env Env, c Culprit) (ClearResult, error) {
	if !c.Rebuildable {
		return ClearResult{}, fmt.Errorf("%s is not rebuildable; o-df clears only rebuildable culprits (relocate moves it instead)", c.Name)
	}
	before := Probe(ctx, env, c)
	res := ClearResult{Name: c.Name, Before: before.Bytes}
	paths := presentPaths(before)
	if len(paths) == 0 {
		return res, fmt.Errorf("%s has nothing to clear: none of its paths is present", c.Name)
	}
	for _, p := range paths {
		if err := guard(env, p); err != nil {
			return res, err
		}
	}
	if len(c.Clear) > 0 {
		run, err := env.CommandRunner()
		if err != nil {
			return res, err
		}
		for _, argv := range c.Clear {
			for _, cmd := range expandCommand(argv, paths) {
				out, err := run(ctx, cmd)
				res.Ran = append(res.Ran, strings.Join(cmd, " "))
				if err != nil {
					return res, fmt.Errorf("%s: %w: %s", strings.Join(cmd, " "), err, strings.TrimSpace(string(out)))
				}
			}
		}
	} else {
		for _, p := range paths {
			clear := emptyPath
			if c.RemoveMatches {
				clear = removeEntry
			}
			if err := clear(p); err != nil {
				res.Skipped = append(res.Skipped, p+": "+err.Error())
				continue
			}
			res.Cleared = append(res.Cleared, p)
		}
		if len(res.Cleared) == 0 && len(res.Skipped) > 0 {
			return res, fmt.Errorf("%s: nothing removed; skipped %d: %s", c.Name, len(res.Skipped), strings.Join(res.Skipped, "; "))
		}
	}
	after := Probe(ctx, env, c)
	res.After, res.Freed = after.Bytes, max(0, before.Bytes-after.Bytes)
	return res, nil
}

func presentPaths(r Report) []string {
	var out []string
	for _, f := range r.Found {
		if f.State == "present" {
			out = append(out, f.Path)
		}
	}
	return out
}

// expandCommand is one argv, or one per path when it holds {path}.
func expandCommand(argv, paths []string) [][]string {
	if !strings.Contains(strings.Join(argv, "\x00"), "{path}") {
		return [][]string{argv}
	}
	var out [][]string
	for _, p := range paths {
		cmd := make([]string, len(argv))
		for i, a := range argv {
			cmd[i] = strings.ReplaceAll(a, "{path}", p)
		}
		out = append(out, cmd)
	}
	return out
}

// guard refuses /, the home directory and anything above it.
func guard(env Env, p string) error {
	clean := filepath.Clean(p)
	if clean == "/" || clean == "." {
		return fmt.Errorf("refusing to change %s", clean)
	}
	if env.Home != "" {
		home := filepath.Clean(env.Home)
		if clean == home || strings.HasPrefix(home+"/", clean+"/") {
			return fmt.Errorf("refusing to change %s: it is the home directory or holds it", clean)
		}
	}
	return nil
}

// emptyPath removes a directory's contents (keeping it) or a file. A
// read-only tree (Go's module cache, Bazel's output) is made writable and
// tried again.
func emptyPath(p string) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return removeAll(p)
	}
	names, err := readNames(p)
	if err != nil {
		return err
	}
	var errs []error
	for _, n := range names {
		if err := removeAll(filepath.Join(p, n)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// removeEntry removes one matched path whole; a test replaces it so no real
// removal can fail or run.
var removeEntry = removeAll

func removeAll(p string) error {
	if err := os.RemoveAll(p); err == nil {
		return nil
	}
	_ = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(path, 0o755)
		}
		return nil
	})
	return os.RemoveAll(p)
}

// Move is one relocated path.
type Move struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Bytes  int64  `json:"bytes"`
	Linked bool   `json:"linked,omitempty"`
}

// RelocateResult is what relocate did.
type RelocateResult struct {
	Name  string `json:"name"`
	Dir   string `json:"dir"`
	Moved []Move `json:"moved"`
	Freed int64  `json:"freed"`
}

// RelocateOptions are relocate's flags.
type RelocateOptions struct {
	To   string // the destination root; a path keeps its place under home
	Link bool   // leave a symlink at the old path
	// SameVolume allows a destination on the source's volume (frees
	// nothing; tests).
	SameVolume bool
}

// Relocate copies each present path of a culprit to TO/<path under home>,
// verifies the copy file by file, deletes the source and, with Link,
// leaves a symlink to the copy.
func Relocate(ctx context.Context, env Env, c Culprit, o RelocateOptions) (RelocateResult, error) {
	if o.To == "" {
		return RelocateResult{}, errors.New("relocate needs a destination: --to DIR")
	}
	to, err := filepath.Abs(o.To)
	if err != nil {
		return RelocateResult{}, err
	}
	res := RelocateResult{Name: c.Name, Dir: to}
	r := Probe(ctx, env, c)
	paths := presentPaths(r)
	if len(paths) == 0 {
		return res, fmt.Errorf("%s has nothing to relocate: none of its paths is present", c.Name)
	}
	if err := os.MkdirAll(to, 0o755); err != nil {
		return res, err
	}
	for _, p := range paths {
		if err := guard(env, p); err != nil {
			return res, err
		}
		dest := filepath.Join(to, relUnder(env, p))
		if strings.HasPrefix(dest+"/", p+"/") {
			return res, fmt.Errorf("destination %s is inside %s", dest, p)
		}
		if _, err := os.Lstat(dest); err == nil {
			return res, fmt.Errorf("%s exists; relocate never overwrites", dest)
		}
		if !o.SameVolume && sameVolume(p, to) {
			return res, fmt.Errorf("%s is on the same volume as %s: relocating frees nothing", to, p)
		}
		if err := copyTree(ctx, p, dest); err != nil {
			return res, fmt.Errorf("copy %s: %w (the source is untouched; remove %s before retrying)", p, err, dest)
		}
		if err := sameTree(p, dest); err != nil {
			return res, fmt.Errorf("verify %s: %w (the source is untouched)", p, err)
		}
		if err := removeAll(p); err != nil {
			return res, fmt.Errorf("delete %s after a verified copy: %w", p, err)
		}
		m := Move{From: p, To: dest, Bytes: r.Found[indexOf(r, p)].Usage.Bytes}
		if o.Link {
			if err := os.Symlink(dest, p); err != nil {
				return res, err
			}
			m.Linked = true
		}
		res.Moved = append(res.Moved, m)
		res.Freed += m.Bytes
	}
	return res, nil
}

func indexOf(r Report, p string) int {
	for i, f := range r.Found {
		if f.Path == p {
			return i
		}
	}
	return 0
}

// relUnder is a path relative to home, or without its leading / outside it.
func relUnder(env Env, p string) string {
	if env.Home != "" {
		if rel, err := filepath.Rel(env.Home, p); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	return strings.TrimPrefix(filepath.Clean(p), "/")
}

func sameVolume(a, b string) bool {
	var sa, sb syscall.Stat_t
	if syscall.Lstat(a, &sa) != nil || syscall.Stat(b, &sb) != nil {
		return false
	}
	return sa.Dev == sb.Dev
}

// copyTree copies files, directories and symlinks with their modes and
// modification times.
func copyTree(ctx context.Context, src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case d.IsDir():
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			return os.Chmod(target, info.Mode().Perm()|0o200)
		case info.Mode().IsRegular():
			if err := copyFile(path, target, info); err != nil {
				return err
			}
			return os.Chtimes(target, info.ModTime(), info.ModTime())
		}
		return nil // sockets, fifos and devices are not copied
	})
}

func copyFile(src, dst string, info fs.FileInfo) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm()|0o200)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, info.Mode().Perm())
}

// sameTree compares two trees entry by entry: kind, size, link target.
func sameTree(a, b string) error {
	ma, err := manifest(a)
	if err != nil {
		return err
	}
	mb, err := manifest(b)
	if err != nil {
		return err
	}
	for k, v := range ma {
		if mb[k] != v {
			return fmt.Errorf("%s differs in the copy (%q, copy %q)", k, v, mb[k])
		}
	}
	if len(ma) != len(mb) {
		return fmt.Errorf("the copy has %d entries, the source %d", len(mb), len(ma))
	}
	return nil
}

func manifest(root string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			link, _ := os.Readlink(path)
			out[rel] = "link:" + link
		case d.IsDir():
			out[rel] = "dir"
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			out[rel] = fmt.Sprintf("file:%d", info.Size())
		}
		return nil
	})
	return out, err
}
