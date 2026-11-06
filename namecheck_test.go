package main

import (
	"testing"

	"ofam.dev/ochazuke/namecheck"
	"ofam.dev/ochazuke/nouncheck"
)

// TestNoNewNameBranches holds the non-test Go to namecheck: no new
// comparison, case label or lookup table keyed by a noun, verb, mode,
// motion or parameter name the runtime declares, and no function grows
// or passes complexity 25. The accepted uses are testdata/namecheck.txt,
// which only shrinks.
func TestNoNewNameBranches(t *testing.T) {
	a, err := New(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	namecheck.Check(t, namecheck.Config{
		Vocab:    a.r.NameVocab(),
		Baseline: "testdata/namecheck.txt",
		Dirs:     []string{"."},
	})
}

// TestVerbReferences holds every tracked Go, JSON and Markdown file to the
// verbs the catalog has: a verb the framework purged (rename, move,
// restore, duplicate, patch; the renamed command verbs) fails here until
// the last reference to it is gone. A dated CHANGELOG section is history.
func TestVerbReferences(t *testing.T) {
	a, err := New(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	namecheck.VerbsCheck(t, namecheck.VerbsConfig{
		Catalog: a.r.VerbCatalog(),
		Removed: a.r.RemovedVerbs(),
		Root:    ".",
	})
}

// TestNouns holds every thing noun to the core verbs and a key prop, and
// every command noun off the core verbs' names (Runtime.ValidateNouns).
func TestNouns(t *testing.T) {
	a, err := New(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	nouncheck.Check(t, a.r, nouncheck.Config{Baseline: "testdata/nouncheck.txt"})
}
