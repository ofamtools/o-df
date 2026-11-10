package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"ofam.dev/ochazuke"
	ocli "ofam.dev/ochazuke/cli"
)

// Every test runs against a temp HOME with a fixture of caches; nothing
// touches the real home directory.

func fixture(t *testing.T) Env {
	t.Helper()
	home := t.TempDir()
	write := func(rel string, size int) {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, bytes.Repeat([]byte("x"), size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".npm/_cacache/a", 64<<10)
	write(".npm/_cacache/b/c", 32<<10)
	write(".npm/_npx/x/package.json", 4<<10)
	write("Library/pnpm/store/v10/f", 128<<10)
	write("Library/pnpm/global/tool", 8<<10) // not part of the store row
	write("Downloads/big.zip", 256<<10)
	write("src/one/.turbo/cache/blob", 16<<10)
	write("src/two/.turbo/cache/blob", 16<<10)
	env := Env{Home: home, UserFile: filepath.Join(home, ".config/o-df/culprits.json")}
	rec := &recorder{}
	recorders.Store(home, rec)
	env.Run = rec.run
	userFile(t, env, `{"project_roots": ["~/src"], "culprits": []}`)
	return env
}

// recorder is every fixture's command runner: it records each clear
// command's argv and runs nothing, so no test can reach a real cache
// through a row's clear commands (go clean -modcache acts on the
// process's GOMODCACHE, not the fixture's HOME).
type recorder struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *recorder) run(_ context.Context, argv []string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, slices.Clone(argv))
	return nil, nil
}

// recorders are the fixtures' runners by HOME.
var recorders sync.Map

// ran is the argv the fixture's runner was called with.
func ran(t *testing.T, env Env) [][]string {
	t.Helper()
	v, ok := recorders.Load(env.Home)
	if !ok {
		t.Fatalf("no recorder for %s", env.Home)
	}
	rec := v.(*recorder)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return slices.Clone(rec.calls)
}

func userFile(t *testing.T, env Env, cfg string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(env.UserFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.UserFile, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

func run(t *testing.T, env Env, args ...string) (string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	err := Main(args, env, strings.NewReader("y\n"), &out, &errb)
	return out.String() + errb.String(), err
}

func TestDefaultsLoadAndValidate(t *testing.T) {
	cfg, err := Load(Env{Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// The defaults name no volume and no folder layout: relocate_to and
	// project_roots come from the user's file.
	if len(cfg.Culprits) < 20 || cfg.RelocateTo != "" || len(cfg.ProjectRoots) != 0 {
		t.Fatalf("defaults: %d rows, relocate_to %q, project_roots %v", len(cfg.Culprits), cfg.RelocateTo, cfg.ProjectRoots)
	}
	for _, c := range cfg.Culprits {
		if c.Recommend != "delete" && c.Recommend != "relocate" && c.Recommend != "keep" {
			t.Errorf("%s: advice %q is not delete, relocate or keep", c.Name, c.Recommend)
		}
	}
}

func TestUserFileAddsReplacesAndDisables(t *testing.T) {
	env := fixture(t)
	userFile(t, env, `{"relocate_to": "/elsewhere", "culprits": [
		{"name": "mine", "paths": ["~/Downloads"], "rebuildable": true, "recommend": "delete"},
		{"name": "downloads", "paths": ["~/Downloads"], "rebuildable": false, "recommend": "keep", "notes": "mine now"},
		{"name": "trash", "paths": ["~/.Trash"], "disabled": true}
	]}`)
	cfg, err := Load(env)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RelocateTo != "/elsewhere" {
		t.Errorf("relocate_to %q", cfg.RelocateTo)
	}
	if c, err := cfg.Resolve("mine"); err != nil || c.Source != env.UserFile {
		t.Errorf("mine: %v %+v", err, c)
	}
	if c, _ := cfg.Resolve("downloads"); c.Notes != "mine now" {
		t.Errorf("downloads not replaced: %+v", c)
	}
	if _, err := cfg.Resolve("trash"); err == nil {
		t.Error("trash is disabled but listed")
	}
}

func TestUserFileRejectsUnknownFields(t *testing.T) {
	env := fixture(t)
	userFile(t, env, `{"culprits": [{"name": "x", "paths": ["~/x"], "sise": 1}]}`)
	if _, err := Load(env); err == nil {
		t.Fatal("a misspelt field loaded")
	}
}

func TestProbeSizesGlobsAndAbsent(t *testing.T) {
	env := fixture(t)
	cfg, _ := Load(env)
	env.ProjectRoots = cfg.ProjectRoots
	turbo, _ := cfg.Resolve("turbo-cache")
	r := Probe(context.Background(), env, turbo)
	if !r.Present || r.Files != 2 || r.Bytes < 32<<10 {
		t.Fatalf("turbo: %+v", r)
	}
	npm, _ := cfg.Resolve("npm-cache")
	r = Probe(context.Background(), env, npm)
	if r.Bytes < 100<<10 || r.Files != 3 {
		t.Fatalf("npm: bytes %d files %d", r.Bytes, r.Files)
	}
	bun, _ := cfg.Resolve("bun-cache")
	if r := Probe(context.Background(), env, bun); r.Present || r.Found[0].State != "absent" {
		t.Fatalf("bun: %+v", r)
	}
}

func TestProbeShowsSymlinkAsRelocated(t *testing.T) {
	env := fixture(t)
	target := t.TempDir()
	if err := os.MkdirAll(filepath.Join(env.Home, "Library/Caches"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(env.Home, "Library/Caches/go-build")); err != nil {
		t.Fatal(err)
	}
	cfg, _ := Load(env)
	env.ProjectRoots = cfg.ProjectRoots
	c, _ := cfg.Resolve("go-build-cache")
	r := Probe(context.Background(), env, c)
	if r.Present || r.Found[0].State != "symlink" || r.Found[0].Target != target {
		t.Fatalf("%+v", r.Found)
	}
}

func TestScanJSONSortsBySizeAndHidesAbsent(t *testing.T) {
	env := fixture(t)
	out, err := run(t, env, "--json")
	if err != nil {
		t.Fatal(err, out)
	}
	var res ScanResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err, out)
	}
	var names []string
	for _, r := range res.Culprits {
		names = append(names, r.Name)
	}
	want := []string{"downloads", "pnpm-store", "npm-cache", "turbo-cache"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("culprits %v, want %v", names, want)
	}
	if res.Disk.Size == 0 {
		t.Error("no disk totals")
	}
}

func TestScanTable(t *testing.T) {
	env := fixture(t)
	out, err := run(t, env, "scan")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CULPRIT", "pnpm-store", "~/Library/pnpm/store", "total"} {
		if !strings.Contains(out, want) {
			t.Errorf("scan lacks %q:\n%s", want, out)
		}
	}
}

func TestClearEmptiesRebuildablePathsOnly(t *testing.T) {
	env := fixture(t)
	out, err := run(t, env, "culprit", "clear", "pnpm-store", "--yes")
	if err != nil {
		t.Fatal(err, out)
	}
	if !strings.Contains(out, `"freed"`) {
		t.Errorf("no freed in %s", out)
	}
	names, _ := readNames(filepath.Join(env.Home, "Library/pnpm/store"))
	if len(names) != 0 {
		t.Errorf("store still holds %v", names)
	}
	if _, err := os.Stat(filepath.Join(env.Home, "Library/pnpm/global/tool")); err != nil {
		t.Errorf("global tools were touched: %v", err)
	}
}

func TestClearRefusesNotRebuildable(t *testing.T) {
	env := fixture(t)
	if out, err := run(t, env, "culprit", "clear", "downloads", "--yes"); err == nil {
		t.Fatalf("cleared downloads: %s", out)
	}
	if _, err := os.Stat(filepath.Join(env.Home, "Downloads/big.zip")); err != nil {
		t.Fatal(err)
	}
}

func TestClearAsksWithoutYes(t *testing.T) {
	env := fixture(t)
	var out, errb bytes.Buffer
	err := Main([]string{"culprit", "clear", "npm-cache"}, env, strings.NewReader("n\n"), &out, &errb)
	if err == nil {
		t.Logf("answered no: %s %s", out.String(), errb.String())
	}
	if _, err := os.Stat(filepath.Join(env.Home, ".npm/_cacache/a")); err != nil {
		t.Fatalf("cleared after answering no: %v", err)
	}
}

func TestClearCommandsRunPerPath(t *testing.T) {
	env := fixture(t)
	userFile(t, env, `{"culprits": [{"name": "npm-cache", "paths": ["~/.npm/_cacache", "~/.npm/_npx"], "rebuildable": true,
		"clear": [["tool", "clean", "{path}"]]}]}`)
	if out, err := run(t, env, "culprit", "clear", "npm-cache", "--yes"); err != nil {
		t.Fatal(err, out)
	}
	calls := ran(t, env)
	if len(calls) != 2 {
		t.Fatalf("ran %d times: %v", len(calls), calls)
	}
	for i, rel := range []string{".npm/_cacache", ".npm/_npx"} {
		want := []string{"tool", "clean", filepath.Join(env.Home, rel)}
		if got := calls[i]; got[0] != want[0] || got[1] != want[1] || !strings.HasSuffix(got[2], rel) {
			t.Errorf("call %d = %v, want %v", i, got, want)
		}
	}
}

// Without a runner, a Home that is not the real home directory never
// runs a clear command.
func TestClearCommandRefusedWithoutRunner(t *testing.T) {
	env := fixture(t)
	env.Run = nil
	log := filepath.Join(env.Home, "ran.log")
	userFile(t, env, `{"culprits": [{"name": "npm-cache", "paths": ["~/.npm/_cacache"], "rebuildable": true,
		"clear": [["sh", "-c", "echo ran >> `+log+`"]]}]}`)
	out, err := run(t, env, "culprit", "clear", "npm-cache", "--yes")
	if err == nil || !strings.Contains(out+err.Error(), "refusing to run a clear command") {
		t.Fatalf("ran without a runner: %v %s", err, out)
	}
	if _, err := os.Stat(log); err == nil {
		t.Fatal("the command ran")
	}
}

// --all over present command rows calls the runner with their argv and
// starts no process: PATH is empty, so a real exec would fail them.
func TestClearAllCommandRowsUseRunner(t *testing.T) {
	env := fixture(t)
	t.Setenv("PATH", t.TempDir())
	for _, rel := range []string{"go/pkg/mod/cache/x", ".cache/uv/x", "Library/Developer/CoreSimulator/Devices/x"} {
		p := filepath.Join(env.Home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err := run(t, env, "free", "--all", "--yes")
	if err != nil {
		t.Fatal(err, out)
	}
	var got []string
	for _, c := range ran(t, env) {
		got = append(got, strings.Join(c, " "))
	}
	slices.Sort(got)
	want := []string{"go clean -modcache", "uv cache clean", "xcrun simctl delete unavailable"}
	if !slices.Equal(got, want) {
		t.Fatalf("runner calls %v, want %v", got, want)
	}
	if strings.Contains(out, "executable file not found") {
		t.Fatalf("a process was started: %s", out)
	}
	for _, rel := range []string{"go/pkg/mod/cache/x", ".cache/uv/x", "Library/Developer/CoreSimulator/Devices/x"} {
		if _, err := os.Stat(filepath.Join(env.Home, rel)); err != nil {
			t.Errorf("%s: the fake runner removed nothing, yet: %v", rel, err)
		}
	}
}

func TestGuardRefusesHome(t *testing.T) {
	env := fixture(t)
	userFile(t, env, `{"culprits": [{"name": "home", "paths": ["~"], "rebuildable": true}]}`)
	if out, err := run(t, env, "culprit", "clear", "home", "--yes"); err == nil || !strings.Contains(out+err.Error(), "home directory") {
		t.Fatalf("cleared the home: %v %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(env.Home, "Downloads/big.zip")); err != nil {
		t.Fatal(err)
	}
}

func TestRelocateCopiesVerifiesDeletesAndLinks(t *testing.T) {
	env := fixture(t)
	if err := os.Symlink("big.zip", filepath.Join(env.Home, "Downloads/link")); err != nil {
		t.Fatal(err)
	}
	to := t.TempDir()
	out, err := run(t, env, "culprit", "relocate", "downloads", "--to", to, "--link", "--same-volume", "--yes")
	if err != nil {
		t.Fatal(err, out)
	}
	dest := filepath.Join(to, "Downloads")
	if fi, err := os.Stat(filepath.Join(dest, "big.zip")); err != nil || fi.Size() != 256<<10 {
		t.Fatalf("copy: %v", err)
	}
	if l, _ := os.Readlink(filepath.Join(dest, "link")); l != "big.zip" {
		t.Errorf("symlink not kept: %q", l)
	}
	if l, err := os.Readlink(filepath.Join(env.Home, "Downloads")); err != nil || l != dest {
		t.Fatalf("no link back: %q %v", l, err)
	}
	// A second relocate finds nothing present (the path is a symlink).
	if _, err := run(t, env, "culprit", "relocate", "downloads", "--to", to, "--same-volume", "--yes"); err == nil {
		t.Error("relocated a symlink")
	}
}

func TestRelocateRefusesSameVolumeAndExisting(t *testing.T) {
	env := fixture(t)
	to := t.TempDir()
	if out, err := run(t, env, "culprit", "relocate", "downloads", "--to", to, "--yes"); err == nil || !strings.Contains(out+err.Error(), "same volume") {
		t.Fatalf("same volume allowed: %v %s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(to, "Downloads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, env, "culprit", "relocate", "downloads", "--to", to, "--same-volume", "--yes"); err == nil {
		t.Fatal("overwrote an existing destination")
	}
	if _, err := os.Stat(filepath.Join(env.Home, "Downloads/big.zip")); err != nil {
		t.Fatal(err)
	}
}

func TestTopListsBiggestFirst(t *testing.T) {
	env := fixture(t)
	out, err := run(t, env, "top", env.Home, "--depth", "1", "--min", "1K", "--json")
	if err != nil {
		t.Fatal(err, out)
	}
	var res TopResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err, out)
	}
	if len(res.Entries) < 3 || filepath.Base(res.Entries[0].Path) != "Downloads" && filepath.Base(res.Entries[0].Path) != "big.zip" {
		t.Fatalf("entries %+v", res.Entries)
	}
	for i := 1; i < len(res.Entries); i++ {
		if res.Entries[i].Usage.Bytes > res.Entries[i-1].Usage.Bytes {
			t.Fatal("not sorted")
		}
	}
}

func TestHardLinkCountsOnce(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	if err := os.WriteFile(a, bytes.Repeat([]byte("x"), 64<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(a, filepath.Join(dir, "b")); err != nil {
		t.Fatal(err)
	}
	u, err := Walk(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if u.Bytes > 64<<10+16<<10 {
		t.Fatalf("hard link counted twice: %d", u.Bytes)
	}
}

func TestCulpritVerbs(t *testing.T) {
	env := fixture(t)
	out, err := run(t, env, "culprit", "get", "npm-cache", "--prop", "size")
	if err != nil || !strings.Contains(out, `"human"`) {
		t.Fatalf("size: %v %s", err, out)
	}
	out, err = run(t, env, "culprit", "show", "pnpm-store")
	if err != nil || !strings.Contains(out, `"found"`) {
		t.Fatalf("show: %v %s", err, out)
	}
	out, err = run(t, env, "culprit", "list", "--format", "table")
	if err != nil || !strings.Contains(out, "pnpm-store") {
		t.Fatalf("list: %v %s", err, out)
	}
	if _, err := run(t, env, "culprit", "get", "nope", "--prop", "size"); err == nil {
		t.Fatal("sized an unknown culprit")
	}
}

func TestSmokeFrame(t *testing.T) {
	env := fixture(t)
	out, err := run(t, env, "tui", "--smoke", "--width", "120", "--height", "24")
	if err != nil {
		t.Fatal(err, out)
	}
	for _, want := range []string{"Culprits 4", "Detail", "downloads", "rebuildable"} {
		if !strings.Contains(out, want) {
			t.Errorf("frame lacks %q:\n%s", want, out)
		}
	}
}

func TestMarkedRowsClearTogether(t *testing.T) {
	env := fixture(t)
	a, err := New(env)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.r.Execute(ochazuke.Request{Noun: "culprit", Verb: "list", Target: pageID}); err != nil {
		t.Fatal(err)
	}
	settle(t, a)
	for _, id := range []string{"culprit/npm-cache", "culprit/turbo-cache"} {
		if err := a.list.ToggleMark(id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.r.Execute(ochazuke.Request{Noun: "ochazuke.collection", Verb: "apply", Args: ochazuke.Arguments{"id": culpritsID, "verb": "clear", "confirmed": true}}); err != nil {
		t.Fatal(err)
	}
	settle(t, a)
	for _, p := range []string{".npm/_cacache/a", "src/one/.turbo/cache"} {
		if _, err := os.Stat(filepath.Join(env.Home, p)); err == nil {
			t.Errorf("%s survived", p)
		}
	}
	if _, err := os.Stat(filepath.Join(env.Home, "Library/pnpm/store/v10/f")); err != nil {
		t.Error("an unmarked row was cleared")
	}
}

// The culprit verbs declare the focused row's name (Parameter.From), so the
// help's run of an unbound clear, with no key and no name, clears the
// focused culprit and only it.
func TestCulpritVerbsFillTheNameFromTheRow(t *testing.T) {
	env := fixture(t)
	a, err := New(env)
	if err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"show", "clear", "relocate"} {
		if got := a.r.Operations.At(ochazuke.Key("culprit." + verb)).Parameters["name"].From; got != "row:name" {
			t.Errorf("culprit.%s name From %q", verb, got)
		}
	}
	if _, err := a.r.Execute(ochazuke.Request{Noun: "culprit", Verb: "list", Target: pageID}); err != nil {
		t.Fatal(err)
	}
	settle(t, a)
	a.tui = true
	if err := a.list.Select("culprit/npm-cache"); err != nil {
		t.Fatal(err)
	}
	if err := a.list.Focus(); err != nil {
		t.Fatal(err)
	}
	if err := a.r.RunUnbound(a.r.BaseFocus(), ochazuke.Request{Noun: "culprit", Verb: "clear"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.r.Execute(ochazuke.Request{Noun: "ochazuke.confirm", Verb: "accept"}); err != nil {
		t.Fatal(err)
	}
	settle(t, a)
	if _, err := os.Stat(filepath.Join(env.Home, ".npm/_cacache/a")); err == nil {
		t.Error("the focused culprit survived")
	}
	if _, err := os.Stat(filepath.Join(env.Home, "Library/pnpm/store/v10/f")); err != nil {
		t.Error("an unfocused culprit was cleared")
	}
}

func settle(t *testing.T, a *App) {
	t.Helper()
	if err := ocli.Settle(a.r, nil); err != nil {
		t.Fatal(err)
	}
}

func TestBindingsAreValid(t *testing.T) {
	a, err := New(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if warnings, err := a.r.ValidateBindings(); err != nil || len(warnings) > 0 {
		t.Fatal(err, warnings)
	}
}

func TestParseSizeAndHuman(t *testing.T) {
	for in, want := range map[string]int64{"1G": 1 << 30, "500M": 500 << 20, "1.5K": 1536, "10": 10} {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v", in, got, err)
		}
	}
	if Human(1<<30) != "1.0G" || Human(9_600_000_000) != "8.9G" || Human(12<<30) != "12G" {
		t.Errorf("Human: %s %s %s", Human(1<<30), Human(9_600_000_000), Human(12<<30))
	}
}

// external makes a directory outside HOME look like another volume
// (deviceOf), links ~/src/ext to it and puts a turbo cache in it.
func external(t *testing.T, env Env) string {
	t.Helper()
	ext, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ext, ".turbo/cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ext, ".turbo/cache/blob"), bytes.Repeat([]byte("x"), 512<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ext, filepath.Join(env.Home, "src/ext")); err != nil {
		t.Fatal(err)
	}
	real := deviceOf
	t.Cleanup(func() { deviceOf = real })
	deviceOf = func(p string) (uint64, error) {
		if r, err := filepath.EvalSymlinks(p); err == nil && (r == ext || strings.HasPrefix(r, ext+"/")) {
			return 999, nil
		}
		return real(p)
	}
	return ext
}

func TestExternalVolumeSkippedByDefault(t *testing.T) {
	env := fixture(t)
	ext := external(t, env)
	cfg, _ := Load(env)
	env.ProjectRoots = cfg.ProjectRoots
	turbo, _ := cfg.Resolve("turbo-cache")
	r := Probe(context.Background(), env, turbo)
	var states []string
	for _, f := range r.Found {
		states = append(states, f.State)
	}
	if r.Bytes >= 512<<10 || !strings.Contains(strings.Join(states, ","), "external") {
		t.Fatalf("external path sized: bytes %d states %v", r.Bytes, states)
	}
	// clear never touches it.
	if out, err := run(t, env, "culprit", "clear", "turbo-cache", "--yes"); err != nil {
		t.Fatal(err, out)
	}
	if _, err := os.Stat(filepath.Join(ext, ".turbo/cache/blob")); err != nil {
		t.Fatalf("clear reached the external volume: %v", err)
	}
	// top refuses a PATH there.
	if out, err := run(t, env, "top", filepath.Join(env.Home, "src/ext")); err == nil || !strings.Contains(out+err.Error(), "another volume") {
		t.Fatalf("top read the external volume: %v %s", err, out)
	}
}

func TestExternalOptIn(t *testing.T) {
	env := fixture(t)
	external(t, env)
	cfg, _ := Load(env)
	env.ProjectRoots = cfg.ProjectRoots
	turbo, _ := cfg.Resolve("turbo-cache")
	turbo.External = true
	if r := Probe(context.Background(), env, turbo); r.Bytes < 512<<10 {
		t.Fatalf("row opt-in: bytes %d", r.Bytes)
	}
	userFile(t, env, `{"external": true, "project_roots": ["~/src"], "culprits": []}`)
	out, err := run(t, env, "scan", "--json")
	if err != nil {
		t.Fatal(err, out)
	}
	if !strings.Contains(out, `"state": "present"`) || strings.Contains(out, `"state": "external"`) {
		t.Fatalf("file opt-in ignored: %s", out)
	}
	if _, err := run(t, env, "top", filepath.Join(env.Home, "src/ext"), "--external"); err != nil {
		t.Fatal(err)
	}
}

func TestTopTimeoutIsPartial(t *testing.T) {
	env := fixture(t)
	out, err := run(t, env, "top", env.Home, "--timeout", "1ns", "--json")
	if err != nil {
		t.Fatal(err, out)
	}
	var res TopResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err, out)
	}
	if !res.Partial {
		t.Fatalf("not partial: %+v", res)
	}
}

func TestProjectRootsExpandPlaceholder(t *testing.T) {
	env := fixture(t)
	write := func(rel string) {
		p := filepath.Join(env.Home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, bytes.Repeat([]byte("x"), 8<<10), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("work/three/.turbo/cache/blob")
	write("src/one/node_modules/left-pad/index.js")
	write("work/three/node_modules/left-pad/index.js")
	probe := func(name string) Report {
		t.Helper()
		a, err := New(env)
		if err != nil {
			t.Fatal(err)
		}
		c, err := a.Cfg.Resolve(name)
		if err != nil {
			t.Fatal(err)
		}
		return Probe(context.Background(), a.Env, c)
	}
	// No roots: a per-project row finds nothing.
	userFile(t, env, `{"culprits": []}`)
	if r := probe("turbo-cache"); r.Present || len(r.Found) != 0 {
		t.Fatalf("no roots, yet turbo-cache found %+v", r.Found)
	}
	// Two roots: every row holding {project_root}, the default's and the
	// user's own, expands under each.
	userFile(t, env, `{"project_roots": ["~/src", "$HOME/work/"], "culprits": [
		{"name": "node-modules", "paths": ["{project_root}/*/node_modules"], "rebuildable": true, "recommend": "delete"}]}`)
	for _, name := range []string{"turbo-cache", "node-modules"} {
		r := probe(name)
		var present []string
		for _, f := range r.Found {
			if f.State == "present" {
				present = append(present, f.Path)
			}
		}
		want := map[string]int{"turbo-cache": 3, "node-modules": 2}[name]
		if len(present) != want {
			t.Errorf("%s: present %v, want %d paths", name, present, want)
		}
	}
	userFile(t, env, `{"project_roots": [""], "culprits": []}`)
	if _, err := Load(env); err == nil {
		t.Error("an empty project root loaded")
	}
}

func TestRelocateWithoutDestinationFails(t *testing.T) {
	env := fixture(t)
	out, err := run(t, env, "culprit", "relocate", "downloads", "--yes")
	if err == nil || !strings.Contains(err.Error(), "relocate needs a destination") || !strings.Contains(err.Error(), "relocate_to") {
		t.Fatalf("relocate without a destination: %v %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(env.Home, "Downloads/big.zip")); err != nil {
		t.Fatal(err)
	}
	// It fails before asking: no prompt was printed.
	if strings.Contains(out, "Relocate downloads") {
		t.Errorf("asked before failing: %s", out)
	}
	// relocate_to in the user's file is the destination when --to is not given.
	to := t.TempDir()
	userFile(t, env, `{"relocate_to": "`+to+`", "culprits": []}`)
	if out, err := run(t, env, "culprit", "relocate", "downloads", "--same-volume", "--yes"); err != nil {
		t.Fatal(err, out)
	}
	if _, err := os.Stat(filepath.Join(to, "Downloads/big.zip")); err != nil {
		t.Fatalf("not relocated to relocate_to: %v", err)
	}
}

func TestClearAllSkipsNotRebuildable(t *testing.T) {
	env := fixture(t)
	var out, errb bytes.Buffer
	if err := Main([]string{"culprit", "clear", "--all"}, env, strings.NewReader("y\n"), &out, &errb); err != nil {
		t.Fatal(err, out.String(), errb.String())
	}
	text := out.String() + errb.String()
	// One question names each culprit with its size, the total and what
	// is left out.
	for _, want := range []string{"Clear 3 culprits", "npm-cache", "pnpm-store", "turbo-cache", "Not rebuildable, never included: downloads"} {
		if !strings.Contains(text, want) {
			t.Errorf("clear --all lacks %q:\n%s", want, text)
		}
	}
	if n := strings.Count(text, "Clear 3 culprits"); n != 1 {
		t.Errorf("asked %d times", n)
	}
	var res ClearManyResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatal(err, out.String())
	}
	if len(res.Cleared) != 3 || res.Freed < 200<<10 {
		t.Fatalf("result %+v", res)
	}
	for _, c := range res.Cleared {
		if c.Name == "downloads" || c.Freed == 0 {
			t.Errorf("cleared %+v", c)
		}
	}
	if _, err := os.Stat(filepath.Join(env.Home, "Downloads/big.zip")); err != nil {
		t.Fatalf("--all reached downloads: %v", err)
	}
	if names, _ := readNames(filepath.Join(env.Home, "Library/pnpm/store")); len(names) != 0 {
		t.Errorf("store still holds %v", names)
	}
}

func TestClearAllDeclined(t *testing.T) {
	env := fixture(t)
	var out, errb bytes.Buffer
	if err := Main([]string{"free", "--all"}, env, strings.NewReader("n\n"), &out, &errb); err == nil {
		t.Fatalf("declined, yet: %s", out.String())
	}
	if _, err := os.Stat(filepath.Join(env.Home, ".npm/_cacache/a")); err != nil {
		t.Fatalf("cleared after answering no: %v", err)
	}
}

func TestFreeAndDeleteReachClear(t *testing.T) {
	env := fixture(t)
	out, err := run(t, env, "free", "npm-cache", "pnpm-store", "--yes")
	if err != nil {
		t.Fatal(err, out)
	}
	if !strings.Contains(out, `"cleared"`) || !strings.Contains(out, `"name": "npm-cache"`) || !strings.Contains(out, `"name": "pnpm-store"`) {
		t.Fatalf("free: %s", out)
	}
	for _, p := range []string{".npm/_cacache/a", "Library/pnpm/store/v10/f"} {
		if _, err := os.Stat(filepath.Join(env.Home, p)); err == nil {
			t.Errorf("%s survived free", p)
		}
	}
	if out, err := run(t, env, "delete", "turbo-cache", "--yes"); err != nil || !strings.Contains(out, `"freed"`) {
		t.Fatalf("delete: %v %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(env.Home, "src/one/.turbo/cache/blob")); err == nil {
		t.Error("turbo survived delete")
	}
	// The culprit noun's own aliases.
	for _, verb := range []string{"free", "delete"} {
		if out, err := run(t, env, "culprit", verb, "nope", "--yes"); err == nil || !strings.Contains(out+err.Error(), `no culprit "nope"`) {
			t.Fatalf("culprit %s is not clear: %v %s", verb, err, out)
		}
	}
	// A culprit that is not rebuildable among the names refuses them all.
	env = fixture(t)
	if out, err := run(t, env, "free", "npm-cache", "downloads", "--yes"); err == nil || !strings.Contains(err.Error(), "not rebuildable") {
		t.Fatalf("free with downloads: %v %s", err, out)
	}
	for _, p := range []string{".npm/_cacache/a", "Downloads/big.zip"} {
		if _, err := os.Stat(filepath.Join(env.Home, p)); err != nil {
			t.Errorf("%s was touched: %v", p, err)
		}
	}
	if _, err := run(t, env, "free", "npm-cache", "--all", "--yes"); err == nil {
		t.Error("names and --all together ran")
	}
}

func TestHelpListsFreeBesideScan(t *testing.T) {
	out, err := run(t, fixture(t), "--help")
	if err != nil {
		t.Fatal(err)
	}
	disk, _, _ := strings.Cut(out, "Other:")
	for _, want := range []string{"free, delete", "scan", "top", "culprit"} {
		if !strings.Contains(disk, want) {
			t.Errorf("the Disk group lacks %q:\n%s", want, disk)
		}
	}
}

func TestFreeArgs(t *testing.T) {
	for in, want := range map[string]string{
		"a":                      "a",
		"a b --yes":              `--yes --names ["a","b"]`,
		"--all --yes":            "--all --yes",
		"--format table a b":     `--format table --names ["a","b"]`,
		"a --target df/culprits": "a --target df/culprits",
	} {
		if got := strings.Join(freeArgs(strings.Fields(in)), " "); got != want {
			t.Errorf("freeArgs(%q) = %q, want %q", in, got, want)
		}
	}
}

// D's request (culprit.clear-all) on the page asks once, from the
// sizes the list already read, and y clears every rebuildable row.
func TestClearAllKeyRequest(t *testing.T) {
	env := fixture(t)
	a, err := New(env)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.r.Execute(ochazuke.Request{Noun: "culprit", Verb: "list", Target: pageID}); err != nil {
		t.Fatal(err)
	}
	settle(t, a)
	a.tui = true
	if _, err := a.r.Execute(ochazuke.Request{Noun: "culprit", Verb: "clear-all", Target: pageID}); err != nil {
		t.Fatal(err)
	}
	top := a.r.Instances[a.r.TopModal()]
	if top == nil || top.Noun != "ochazuke.confirm" || !strings.Contains(top.Payload.Prompt, "Clear 3 culprits") || !strings.Contains(top.Payload.Prompt, "downloads") {
		t.Fatalf("no confirm naming the culprits: %+v", top)
	}
	if _, err := a.r.Execute(ochazuke.Request{Noun: "ochazuke.confirm", Verb: "accept"}); err != nil {
		t.Fatal(err)
	}
	settle(t, a)
	for _, p := range []string{".npm/_cacache/a", "Library/pnpm/store/v10/f", "src/one/.turbo/cache/blob"} {
		if _, err := os.Stat(filepath.Join(env.Home, p)); err == nil {
			t.Errorf("%s survived", p)
		}
	}
	if _, err := os.Stat(filepath.Join(env.Home, "Downloads/big.zip")); err != nil {
		t.Fatal(err)
	}
}

// A row's paths_from command (go env GOCACHE) supplies its paths, so a
// GOCACHE elsewhere is listed, sized and cleared; a failing command falls
// back to the static paths.
func TestPathsFromCommandSuppliesPaths(t *testing.T) {
	env := fixture(t)
	cache := t.TempDir()
	if err := os.WriteFile(filepath.Join(cache, "blob"), bytes.Repeat([]byte("x"), 96<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	var asked [][]string
	env.Query = func(_ context.Context, argv []string) ([]byte, error) {
		asked = append(asked, slices.Clone(argv))
		return []byte(cache + "\n"), nil
	}
	out, err := run(t, env, "culprit", "show", "go-build-cache")
	if err != nil {
		t.Fatal(err, out)
	}
	if !strings.Contains(out, cache) {
		t.Errorf("show lacks the GOCACHE path %s:\n%s", cache, out)
	}
	cfg, err := Load(env)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(cfg.Culprits, func(c Culprit) bool { return c.Name == "go-build-cache" })
	r := Probe(context.Background(), env, cfg.Culprits[i])
	if !r.Present || r.Bytes < 96<<10 || len(r.Found) != 2 || r.Found[0].Path == r.Found[1].Path || (r.Found[0].Path != cache && r.Found[1].Path != cache) {
		t.Fatalf("probe %+v", r)
	}
	if len(asked) == 0 || strings.Join(asked[0], " ") != "go env GOCACHE" {
		t.Errorf("asked %v", asked)
	}
	// free: the dry run targets the path; clear runs the tool's cleaner.
	if _, err := run(t, env, "free", "go-build-cache", "--yes"); err != nil {
		t.Fatal(err)
	}
	calls := ran(t, env)
	if len(calls) != 2 || strings.Join(calls[0], " ") != "go clean -cache" || calls[1][0] != "sh" {
		t.Errorf("ran %v", calls)
	}
}

func TestPathsFromFailureFallsBackToStaticPaths(t *testing.T) {
	env := fixture(t)
	env.Query = func(context.Context, []string) ([]byte, error) { return nil, os.ErrNotExist }
	cfg, _ := Load(env)
	i := slices.IndexFunc(cfg.Culprits, func(c Culprit) bool { return c.Name == "go-build-cache" })
	r := Probe(context.Background(), env, cfg.Culprits[i])
	if len(r.Found) != 1 || !strings.HasSuffix(r.Found[0].Path, "Library/Caches/go-build") {
		t.Fatalf("found %+v", r.Found)
	}
}

// Both the tool's own location and the default one are listed; a command
// printing the same path as a static one counts it once.
func TestPathsFromDedupesAgainstStaticPaths(t *testing.T) {
	env := fixture(t)
	env.Query = func(context.Context, []string) ([]byte, error) {
		return []byte(filepath.Join(env.Home, "Library/Caches/go-build") + "\n"), nil
	}
	cfg, _ := Load(env)
	i := slices.IndexFunc(cfg.Culprits, func(c Culprit) bool { return c.Name == "go-build-cache" })
	if r := Probe(context.Background(), env, cfg.Culprits[i]); len(r.Found) != 1 {
		t.Fatalf("found %+v", r.Found)
	}
}

// min_age, skip, skip_with and remove_matches are data on a row: only old,
// non-live, non-checkout scratch entries are listed as present and cleared,
// each removed whole.
func TestScratchRowsHonourAgeSkipAndCheckouts(t *testing.T) {
	env := fixture(t)
	tmp := t.TempDir()
	env.Tmp = tmp
	old := env.now().Add(-72 * time.Hour)
	mk := func(rel string, mtime time.Time) {
		p := filepath.Join(tmp, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, bytes.Repeat([]byte("x"), 8<<10), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(p, mtime, mtime)
		_ = os.Chtimes(filepath.Dir(p), mtime, mtime)
	}
	mk("lane-old/out/a", old)
	mk("lane-young/a", env.now())
	mk("lane-git/repo/.git/HEAD", old)
	mk("claude-keep/a", old)
	mk("install-family-x-1", old)
	mk("install-family-x-2", env.now())
	if _, err := run(t, env, "free", "lane-scratch", "--yes"); err != nil {
		t.Fatal(err)
	}
	gone := func(rel string) bool { _, err := os.Lstat(filepath.Join(tmp, rel)); return err != nil }
	if !gone("lane-old") {
		t.Error("old scratch dir kept")
	}
	for _, keep := range []string{"lane-young", "lane-git", "claude-keep", "install-family-x-1"} {
		if gone(keep) {
			t.Errorf("%s removed", keep)
		}
	}
	if _, err := run(t, env, "free", "install-family-builds", "--yes"); err != nil {
		t.Fatal(err)
	}
	if !gone("install-family-x-1") || gone("install-family-x-2") {
		t.Error("installer builds: want only the old one removed")
	}
}

// A server a running process names is never offered, whatever its age; if
// the listing cannot be asked, nothing is offered.
func TestSkipFromKeepsRunningServers(t *testing.T) {
	env := fixture(t)
	old := env.now().Add(-72 * time.Hour)
	for _, n := range []string{"live-idle", "dead-one"} {
		p := filepath.Join(env.Home, "Library/Application Support/tuios-servers", n, "state")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(p, old, old)
		_ = os.Chtimes(filepath.Dir(p), old, old)
	}
	env.Query = func(_ context.Context, argv []string) ([]byte, error) {
		if argv[0] == "sh" {
			return []byte("live-idle\n"), nil
		}
		return nil, os.ErrNotExist
	}
	if _, err := run(t, env, "free", "tuios-server-dirs", "--yes"); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(env.Home, "Library/Application Support/tuios-servers")
	if _, err := os.Stat(filepath.Join(base, "live-idle")); err != nil {
		t.Error("running server's dir removed")
	}
	if _, err := os.Stat(filepath.Join(base, "dead-one")); err == nil {
		t.Error("dead server's dir kept")
	}
	env.Query = func(context.Context, []string) ([]byte, error) { return nil, os.ErrNotExist }
	cfg, _ := Load(env)
	i := slices.IndexFunc(cfg.Culprits, func(c Culprit) bool { return c.Name == "tuios-server-dirs" })
	if r := Probe(context.Background(), env, cfg.Culprits[i]); r.Present {
		t.Errorf("failed listing still offers %+v", r.Found)
	}
}

// lane-scratch is narrowed by data: only entries the user owns, directories
// and plain files, nothing hidden, no sockets, no lock files; others are
// listed as skipped and never removed.
func TestScratchRowLeavesOtherAppsAlone(t *testing.T) {
	env := fixture(t)
	tmp := t.TempDir()
	env.Tmp = tmp
	old := env.now().Add(-72 * time.Hour)
	mkfile := func(rel string) string {
		p := filepath.Join(tmp, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, bytes.Repeat([]byte("x"), 8<<10), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(p, old, old)
		_ = os.Chtimes(filepath.Dir(p), old, old)
		return p
	}
	mkfile("lane-mine/a")
	mkfile("loose-file")
	mkfile("theirs/a")
	mkfile(".hidden.dylib")
	mkfile("MozillaUpdateLock-ABC")
	mkfile("thing.lock")
	sock := filepath.Join(tmp, "app.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip("no unix sockets:", err)
	}
	defer l.Close()
	_ = os.Chtimes(sock, old, old)
	mkfile("sock-named-dir/a")
	env.Mine = func(fi fs.FileInfo) bool { return fi.Name() != "theirs" }

	cfg, err := Load(env)
	if err != nil {
		t.Fatal(err)
	}
	c, err := cfg.Resolve("lane-scratch")
	if err != nil {
		t.Fatal(err)
	}
	rep := Probe(context.Background(), env, c)
	state := map[string]string{}
	for _, f := range rep.Found {
		state[filepath.Base(f.Path)] = f.State
	}
	for _, n := range []string{"lane-mine", "loose-file", "sock-named-dir"} {
		if state[n] != "present" {
			t.Errorf("%s: state %q, want present", n, state[n])
		}
	}
	for _, n := range []string{"theirs", ".hidden.dylib", "MozillaUpdateLock-ABC", "thing.lock", "app.sock"} {
		if state[n] != "skipped" {
			t.Errorf("%s: state %q, want skipped", n, state[n])
		}
	}
	out, err := run(t, env, "show", "lane-scratch")
	if err != nil || !strings.Contains(out, "lane-mine") || !strings.Contains(out, "skipped") {
		t.Errorf("show: %v\n%s", err, out)
	}
}

// types and skip_hidden are plain row data on any culprit.
func TestTypesAndHiddenAreRowData(t *testing.T) {
	env := fixture(t)
	dir := filepath.Join(env.Home, "pile")
	for _, n := range []string{"d", ".h"} {
		if err := os.MkdirAll(filepath.Join(dir, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := Culprit{Name: "pile", Paths: []string{"~/pile/*"}, Rebuildable: true, Types: []string{"file"}, SkipHidden: true, Owner: "self"}
	rep := Probe(context.Background(), env, c)
	got := map[string]string{}
	for _, f := range rep.Found {
		got[filepath.Base(f.Path)] = f.State
	}
	if got["f"] != "present" || got["d"] != "skipped" || got[".h"] != "skipped" {
		t.Errorf("states %v", got)
	}
	env.Mine = func(fs.FileInfo) bool { return false }
	if r := Probe(context.Background(), env, c); r.Present {
		t.Error("owner self with nothing owned: want none present")
	}
	if err := validate(Config{Culprits: []Culprit{{Name: "x", Paths: []string{"/x"}, Owner: "root"}}}); err == nil {
		t.Error("owner root accepted")
	}
	if err := validate(Config{Culprits: []Culprit{{Name: "x", Paths: []string{"/x"}, Types: []string{"pipe"}}}}); err == nil {
		t.Error("unknown type accepted")
	}
}

// A path that cannot be removed is skipped and reported; the rest are
// still removed and the clear does not abort.
func TestClearContinuesPastPermissionError(t *testing.T) {
	env := fixture(t)
	dir := filepath.Join(env.Home, "pile")
	for _, n := range []string{"a", "b", "c"} {
		if err := os.MkdirAll(filepath.Join(dir, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var tried []string
	old := removeEntry
	defer func() { removeEntry = old }()
	removeEntry = func(p string) error {
		tried = append(tried, filepath.Base(p))
		if filepath.Base(p) == "b" {
			return &fs.PathError{Op: "unlinkat", Path: p, Err: syscall.EACCES}
		}
		return nil
	}
	c := Culprit{Name: "pile", Paths: []string{"~/pile/*"}, Rebuildable: true, RemoveMatches: true}
	res, err := Clear(context.Background(), env, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Cleared) != 2 || len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0], "permission denied") {
		t.Errorf("cleared %v skipped %v", res.Cleared, res.Skipped)
	}
	if !slices.Equal(tried, []string{"a", "b", "c"}) {
		t.Errorf("tried %v", tried)
	}
	removeEntry = func(p string) error { return syscall.EACCES }
	if _, err := Clear(context.Background(), env, c); err == nil {
		t.Error("every path failing: want an error")
	}
}
