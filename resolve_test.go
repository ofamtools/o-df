package main

import (
	"bytes"
	"encoding/json"
	"ofam.dev/ochazuke"
	ocli "ofam.dev/ochazuke/cli"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Culprit names resolve like oden's command words: exact, alias, unique
// prefix (resolve.go). Every test runs on the temp HOME fixture plus the
// Go build and Homebrew caches. Clear commands go to the fixture's
// recording runner, never to a process.

func goFixture(t *testing.T) Env {
	t.Helper()
	env := fixture(t)
	for _, rel := range []string{"Library/Caches/go-build/aa/x", "Library/Caches/Homebrew/x.tar.gz"} {
		p := filepath.Join(env.Home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, bytes.Repeat([]byte("x"), 8<<10), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return env
}

func present(t *testing.T, env Env, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		if _, err := os.Stat(filepath.Join(env.Home, rel)); err != nil {
			t.Errorf("%s was touched: %v", rel, err)
		}
	}
}

func gone(t *testing.T, env Env, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		if _, err := os.Stat(filepath.Join(env.Home, rel)); err == nil {
			t.Errorf("%s survived", rel)
		}
	}
}

func TestResolveExactWins(t *testing.T) {
	env := goFixture(t)
	// A row named go is exact, so go is no longer ambiguous; go-build-cache
	// and go-mod-cache are its descendants and never make go- ambiguous
	// against it either (oden's rule).
	userFile(t, env, `{"culprits": [{"name": "go", "paths": ["~/go"], "rebuildable": false}]}`)
	cfg, err := Load(env)
	if err != nil {
		t.Fatal(err)
	}
	for word, want := range map[string]string{"go": "go", "npm-cache": "npm-cache", "go-build-cache": "go-build-cache"} {
		if cu, err := cfg.Resolve(word); err != nil || cu.Name != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", word, cu.Name, err, want)
		}
	}
	// An exact name keeps the verb's own confirm, unchanged.
	var out, errb bytes.Buffer
	if err := Main([]string{"culprit", "clear", "npm-cache"}, env, strings.NewReader("n\n"), &out, &errb); err == nil {
		t.Fatal("declined, yet ran")
	}
	if text := errb.String(); !strings.Contains(text, "Clear npm-cache? Its tool rebuilds it on demand. [y/N]") || strings.Contains(text, " is ") {
		t.Errorf("exact confirm: %s", text)
	}
}

func TestResolveUniquePrefixAndAlias(t *testing.T) {
	env := goFixture(t)
	out, err := run(t, env, "culprit", "show", "go-b")
	if err != nil || !strings.Contains(out, `"name": "go-build-cache"`) {
		t.Fatalf("show go-b: %v %s", err, out)
	}
	if out, err := run(t, env, "culprit", "get", "brew", "--prop", "size"); err != nil || !strings.Contains(out, `"name": "homebrew-cache"`) {
		t.Fatalf("size brew: %v %s", err, out)
	}
	// The confirm names the full culprit and the word it came from.
	var o, e bytes.Buffer
	if err := Main([]string{"free", "npm"}, env, strings.NewReader("n\n"), &o, &e); err == nil {
		t.Fatal("declined, yet ran")
	}
	if !strings.Contains(e.String(), `Clear npm-cache? Its tool rebuilds it on demand. ("npm" is npm-cache)`) {
		t.Errorf("abbreviated confirm: %s", e.String())
	}
	present(t, env, ".npm/_cacache/a")
	if out, err := run(t, env, "free", "npm"); err != nil || !strings.Contains(out, `"name": "npm-cache"`) {
		t.Fatalf("free npm: %v %s", err, out)
	}
	gone(t, env, ".npm/_cacache/a")
	if out, err := run(t, env, "free", "brew", "--yes"); err != nil || !strings.Contains(out, `"name": "homebrew-cache"`) {
		t.Fatalf("free brew: %v %s", err, out)
	}
	gone(t, env, "Library/Caches/Homebrew/x.tar.gz")
}

func TestResolveAmbiguousNeverActs(t *testing.T) {
	env := goFixture(t)
	const want = `"go" is ambiguous: go-build-cache, go-mod-cache`
	for _, args := range [][]string{
		{"culprit", "get", "go", "--prop", "size"},
		{"culprit", "clear", "go", "--yes"},
		{"free", "go", "--yes"},
		{"delete", "go", "--yes"},
		{"culprit", "relocate", "go", "--to", t.TempDir(), "--yes"},
	} {
		out, err := run(t, env, args...)
		if err == nil || !strings.Contains(out+err.Error(), want) {
			t.Errorf("%v: %v %s", args, err, out)
		}
	}
	present(t, env, "Library/Caches/go-build/aa/x")
}

func TestResolveUnknownSuggests(t *testing.T) {
	env := goFixture(t)
	out, err := run(t, env, "culprit", "get", "gp-build-cache", "--prop", "size")
	if err == nil || !strings.Contains(out+err.Error(), `no culprit "gp-build-cache"; did you mean: go-build-cache`) {
		t.Fatalf("typo: %v %s", err, out)
	}
	out, err = run(t, env, "free", "zzz", "--yes")
	if err == nil || !strings.Contains(out+err.Error(), `no culprit "zzz" (o-df culprit list names them)`) {
		t.Fatalf("unknown: %v %s", err, out)
	}
	if _, err := run(t, env, "culprit", "relocate", "zzz", "--to", t.TempDir(), "--yes"); err == nil {
		t.Fatal("relocated an unknown culprit")
	}
}

func TestResolveSeveralNames(t *testing.T) {
	// Exact, prefix and alias together: one confirm listing full names.
	env := goFixture(t)
	var o, e bytes.Buffer
	if err := Main([]string{"free", "pnpm-store", "npm", "brew"}, env, strings.NewReader("y\n"), &o, &e); err != nil {
		t.Fatal(err, o.String(), e.String())
	}
	for _, want := range []string{"Clear 3 culprits", "  homebrew-cache", "  npm-cache", "  pnpm-store"} {
		if !strings.Contains(e.String()+o.String(), want) {
			t.Errorf("prompt lacks %q:\n%s", want, e.String())
		}
	}
	gone(t, env, ".npm/_cacache/a", "Library/pnpm/store/v10/f", "Library/Caches/Homebrew/x.tar.gz")

	// One ambiguous or unknown word refuses them all, naming each bad word.
	env = goFixture(t)
	out, err := run(t, env, "free", "npm", "go", "zzz", "--yes")
	if err == nil {
		t.Fatalf("ran: %s", out)
	}
	for _, want := range []string{`"go" is ambiguous: go-build-cache, go-mod-cache`, `no culprit "zzz"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	present(t, env, ".npm/_cacache/a", "Library/Caches/go-build/aa/x")

	// Two words for one culprit ask about one culprit, singular.
	env = goFixture(t)
	var o1, e1 bytes.Buffer
	if err := Main([]string{"free", "npm", "npm-cache"}, env, strings.NewReader("n\n"), &o1, &e1); err == nil {
		t.Fatal("declined, yet ran")
	}
	if !strings.Contains(e1.String(), "Clear 1 culprit, ") || strings.Contains(e1.String(), "1 culprits") {
		t.Errorf("singular prompt: %s", e1.String())
	}

	// --names JSON resolves the same way, and two words for one culprit
	// clear it once.
	env = goFixture(t)
	out, err = run(t, env, "culprit", "clear", "--names", `["npm","npm-cache","go-b"]`, "--yes")
	if err != nil || strings.Count(out, `"name": "npm-cache"`) != 1 || !strings.Contains(out, `"name": "go-build-cache"`) {
		t.Fatalf("--names: %v %s", err, out)
	}
	// go-build-cache clears with the tool's own cleaner (the fixture's
	// runner records it), not by removing the directory.
	gone(t, env, ".npm/_cacache/a")
	if calls := ran(t, env); len(calls) != 2 || strings.Join(calls[0], " ") != "go clean -cache" {
		t.Errorf("ran %v", calls)
	}
	env = goFixture(t)
	if out, err := run(t, env, "culprit", "clear", "--names", `["go"]`, "--yes"); err == nil {
		t.Fatalf("--names [go] ran: %s", out)
	}
	present(t, env, "Library/Caches/go-build/aa/x")
}

func TestResolveLeavesAllAlone(t *testing.T) {
	env := goFixture(t)
	var o, e bytes.Buffer
	if err := Main([]string{"free", "--all"}, env, strings.NewReader("y\n"), &o, &e); err != nil {
		t.Fatal(err, o.String(), e.String())
	}
	// Every present rebuildable culprit by its full name; downloads never.
	text := e.String() + o.String()
	if strings.Contains(text, "go-mod-cache") {
		t.Fatalf("go-mod-cache is absent, yet listed:\n%s", text)
	}
	for _, want := range []string{"Clear 5 culprits", "go-build-cache", "homebrew-cache", "npm-cache", "pnpm-store", "turbo-cache", "Not rebuildable, never included: downloads"} {
		if !strings.Contains(text, want) {
			t.Errorf("--all lacks %q:\n%s", want, text)
		}
	}
	present(t, env, "Downloads/big.zip")
	if _, err := run(t, env, "free", "npm", "--all", "--yes"); err == nil {
		t.Error("a name and --all together ran")
	}
}

func TestNameChoicesForPalette(t *testing.T) {
	env := goFixture(t)
	a, err := New(env)
	if err != nil {
		t.Fatal(err)
	}
	param := a.r.Operations.At(ochazuke.Key(a.r.CanonicalKey("culprit.clear"))).Parameters["name"]
	ids := func(word string) []string {
		choices, err := a.r.ChoicesFor(a.r.Context(), param, word)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range choices {
			out = append(out, c.ID)
		}
		return out
	}
	for word, want := range map[string][]string{
		"go":        {"go-build-cache", "go-mod-cache"},
		"go-b":      {"go-build-cache"},
		"brew":      {"homebrew-cache"},
		"npm-cache": {"npm-cache"},
		"zzz":       nil,
	} {
		if got := ids(word); !slices.Equal(got, want) {
			t.Errorf("choices for %q = %v, want %v", word, got, want)
		}
	}
	if got := ids(""); len(got) != len(a.Cfg.Culprits) {
		t.Errorf("no word lists %d of %d", len(got), len(a.Cfg.Culprits))
	}
}

func TestCatalogDeclaresSingularNamespace(t *testing.T) {
	var doc ocli.CatalogDoc
	if err := json.Unmarshal([]byte(catalogJSON(t)), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Namespace != "df" || ochazuke.DefaultNamespace != "df" {
		t.Fatalf("catalog namespace %q, default %q; want df", doc.Namespace, ochazuke.DefaultNamespace)
	}
	if got, err := ocli.CatalogNamespace(doc, "o-df"); err != nil || got != "df" {
		t.Fatal(got, err)
	}
}

func catalogJSON(t *testing.T) string {
	t.Helper()
	var out, errb bytes.Buffer
	if err := Main([]string{"catalog"}, goFixture(t), strings.NewReader(""), &out, &errb); err != nil {
		t.Fatal(err, errb.String())
	}
	return out.String()
}
