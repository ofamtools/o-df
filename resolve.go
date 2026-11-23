package main

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Culprit names resolve the way oden resolves command words: an exact name
// or alias wins, else the one culprit with a name or alias starting with
// the word. A descendant never makes a prefix ambiguous (oden's hiiro rule:
// with rows npm and npm-cache, np is npm). A prefix of several is
// ambiguous and names them; a word naming none lists the closest names.
// Every place that takes a culprit name (the command line, --names, the
// palette's choices for name) goes through Resolve, so a verb never acts
// on a name it could not pin to one row.

// Resolve is the culprit word names.
func (cfg Config) Resolve(word string) (Culprit, error) {
	res := cfg.resolve(word)
	switch {
	case res.name != "":
		return cfg.byName(res.name), nil
	case res.ambiguous:
		return Culprit{}, fmt.Errorf("%q is ambiguous: %s", word, strings.Join(res.candidates, ", "))
	}
	if s := cfg.Suggest(word); len(s) > 0 {
		return Culprit{}, fmt.Errorf("no culprit %q; did you mean: %s (o-df culprit list names them)", word, strings.Join(s, ", "))
	}
	return Culprit{}, fmt.Errorf("no culprit %q (o-df culprit list names them)", word)
}

// ResolveAll resolves every word, once each (two words naming one culprit
// give it once), or fails naming every word that did not resolve.
func (cfg Config) ResolveAll(words []string) ([]Culprit, error) {
	var out []Culprit
	var errs []error
	for _, w := range words {
		cu, err := cfg.Resolve(w)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !slices.ContainsFunc(out, func(c Culprit) bool { return c.Name == cu.Name }) {
			out = append(out, cu)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return out, nil
}

// Candidates are the culprits word may name, for completion: every
// culprit for no word, the one it resolves to, the ones a prefix shares
// (the ambiguous candidates), else none.
func (cfg Config) Candidates(word string) []Culprit {
	if word == "" {
		return slices.Clone(cfg.Culprits)
	}
	var out []Culprit
	for _, n := range cfg.resolve(word).candidates {
		out = append(out, cfg.byName(n))
	}
	return out
}

// resolution is what a word names: one culprit (name), or the candidates
// an ambiguous prefix shares.
type resolution struct {
	name       string
	candidates []string
	ambiguous  bool
}

// tokens maps every name and alias to its culprit's name. Names win over
// aliases; a later row's alias that another row already claims is
// shadowed (oden's catalog rule, copied from ~/proj/oden/catalog.go).
func (cfg Config) tokens() map[string]string {
	t := make(map[string]string, len(cfg.Culprits))
	for _, c := range cfg.Culprits {
		t[c.Name] = c.Name
	}
	for _, c := range cfg.Culprits {
		for _, a := range c.Aliases {
			if _, taken := t[a]; !taken {
				t[a] = c.Name
			}
		}
	}
	return t
}

// resolve matches word as oden's Catalog.Resolve does: an exact name or
// alias wins; else every culprit with a name or alias starting with word,
// minus descendants (a name continuing another candidate's name with "-").
func (cfg Config) resolve(word string) resolution {
	if word == "" {
		return resolution{}
	}
	t := cfg.tokens()
	if n, ok := t[word]; ok {
		return resolution{name: n, candidates: []string{n}}
	}
	seen := map[string]bool{}
	var matches []string
	for tok, n := range t {
		if strings.HasPrefix(tok, word) && !seen[n] {
			seen[n] = true
			matches = append(matches, n)
		}
	}
	var kept []string
	for _, e := range matches {
		nested := false
		for _, o := range matches {
			if o != e && strings.HasPrefix(e, o+"-") {
				nested = true
				break
			}
		}
		if !nested {
			kept = append(kept, e)
		}
	}
	sort.Strings(kept)
	res := resolution{candidates: kept}
	switch len(kept) {
	case 0:
	case 1:
		res.name = kept[0]
	default:
		res.ambiguous = true
	}
	return res
}

func (cfg Config) byName(name string) Culprit {
	i := slices.IndexFunc(cfg.Culprits, func(c Culprit) bool { return c.Name == name })
	return cfg.Culprits[i]
}

// maxSuggestions caps the "did you mean" list.
const maxSuggestions = 3

// Suggest lists the culprits word may have meant, at most three, for a
// word that names none: first those whose name or alias is a prefix of
// word (go-build-caches → go-build-cache), then those within a small edit
// distance (one edit for a word of up to five letters, else two). Copied
// from oden's App.Suggest (~/proj/oden/notfound.go), which works on an
// App's catalog and is not callable on a plain one.
func (cfg Config) Suggest(word string) []string {
	if word == "" {
		return nil
	}
	var prefixed, close []string
	limit := 1
	if len(word) > 5 {
		limit = 2
	}
	for _, c := range cfg.Culprits {
		isPrefix, best := false, limit+1
		for _, t := range append([]string{c.Name}, c.Aliases...) {
			if strings.HasPrefix(word, t) {
				isPrefix = true
			}
			best = min(best, editDistance(word, t))
		}
		switch {
		case isPrefix:
			prefixed = append(prefixed, c.Name)
		case best <= limit:
			close = append(close, c.Name)
		}
	}
	out := append(prefixed, close...)
	if len(out) > maxSuggestions {
		out = out[:maxSuggestions]
	}
	return out
}

// editDistance is the optimal string alignment distance between a and b:
// insertions, deletions, substitutions and adjacent transpositions.
// Copied from oden (~/proj/oden/notfound.go), where it is unexported.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	d := make([][]int, len(ra)+1)
	for i := range d {
		d[i] = make([]int, len(rb)+1)
		d[i][0] = i
	}
	for j := range d[0] {
		d[0][j] = j
	}
	for i := 1; i <= len(ra); i++ {
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j]+1, d[i][j-1]+1, d[i-1][j-1]+cost)
			if i > 1 && j > 1 && ra[i-1] == rb[j-2] && ra[i-2] == rb[j-1] {
				d[i][j] = min(d[i][j], d[i-2][j-2]+1)
			}
		}
	}
	return d[len(ra)][len(rb)]
}
