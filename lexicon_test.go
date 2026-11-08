package main

import (
	"testing"

	"ofam.dev/ochazuke/lexiconcheck"
)

// o-df's registered operations are held to the lexicon's naming rules
// (lexicon plan): one word has one part of speech, nouns are singular, a
// namespace is not a noun or action word, a key has at most three parts.
// testdata/lexicon-words.tsv is a copy of ochazuke's word list.
func TestLexicon(t *testing.T) {
	a, err := New(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	lexiconcheck.Check(t, lexiconcheck.Config{
		Operations: &a.r.Operations,
		MainNouns:  map[string]string{"df": "culprit"},
		Words:      "testdata/lexicon-words.tsv",
		Baseline:   "testdata/lexiconcheck.txt",
	})
}
