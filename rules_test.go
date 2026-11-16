package main

import (
	"testing"

	"ofam.dev/ochazuke/rulecheck"
)

// TestRules runs ochazuke's hiddenkeys and deadkeys checks over o-df's
// runtime: a hidden default row may not take a key that did something at
// the same place without it. testdata/rules/places.json holds the places
// of every noun as a running mtm recorded them (noun, ancestors, modal),
// so the pages, trees and forms the fixture does not build are walked
// too. The other checks have their own tests and baselines.
func TestRules(t *testing.T) {
	a, err := New(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	places, err := rulecheck.LoadPlaces("testdata/rules/places.json")
	if err != nil {
		t.Fatal(err)
	}
	rulecheck.Check(t, rulecheck.Config{
		Runtime: a.r,
		Places:  places,
		Off: map[string]string{
			"names":    "TestNoNewNameBranches holds it",
			"refusals": "no refusecheck baseline here; not part of this check",
			"lexicon":  "TestLexicon holds it",
			"nouns":    "TestNouns holds it",
		},
	})
}
