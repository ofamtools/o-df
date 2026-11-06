package main

import (
	"ofam.dev/ochazuke"
)

// The culprit noun is a thing noun over the culprits file's rows, read
// only: a facts store (the file is edited by hand; clear and relocate are
// the actions). The standard list and get answer the rows' definitions
// without sizing anything; the named verbs list, show and size keep the
// sized reports (they run a probe off the loop, which a store call does
// not). The clear verb is also the delete core verb (its alias), so it
// keeps asking before it empties anything.

func culpritProps() []ochazuke.PropSpec {
	text := func(desc string) ochazuke.Parameter { return ochazuke.Parameter{Type: "string", Description: desc} }
	return []ochazuke.PropSpec{
		{Name: "name", Key: true, Parameter: text("The culprit's name (an alias or a unique prefix names it too)")},
		{Name: "title", Role: ochazuke.RoleLabel, Fallback: "name", Parameter: text("What the row calls itself")},
		{Name: "category", Parameter: text("The kind of cache or data")},
		{Name: "aliases", Parameter: ochazuke.Parameter{Type: ochazuke.SettingList, Description: "More words that name the row"}},
		{Name: "paths", Parameter: ochazuke.Parameter{Type: ochazuke.SettingList, Description: "The paths the row sizes (~-relative or absolute)"}},
		{Name: "rebuildable", Parameter: ochazuke.Parameter{Type: ochazuke.SettingBoolean, Description: "Whether the owning tool recreates the data on demand (only such a row can be cleared)"}},
		{Name: "rebuild", Parameter: text("How the data comes back")},
		{Name: "recommend", Parameter: text("The advice the scan prints: delete, relocate or keep")},
		{Name: "notes", Parameter: text("Notes on the row")},
		{Name: "source", Parameter: text("Where the row came from: default or the user file")},
	}
}

func (a *App) culpritStore() ochazuke.Store {
	return ochazuke.Store{
		Kind:  ochazuke.Collection,
		Facts: true,
		Get: func(_ ochazuke.Ctx, name string) (ochazuke.Thing, error) {
			cu, err := a.Cfg.Resolve(name)
			if err != nil {
				return nil, err
			}
			return ochazuke.ThingOf(cu)
		},
		List: func(_ ochazuke.Ctx, _ ochazuke.ListQuery) ([]ochazuke.Thing, error) {
			out := make([]ochazuke.Thing, 0, len(a.Cfg.Culprits))
			for _, cu := range a.Cfg.Culprits {
				t, err := ochazuke.ThingOf(cu)
				if err != nil {
					return nil, err
				}
				out = append(out, t)
			}
			return out, nil
		},
	}
}

// defineCoreDelete makes the core delete verb the clear verb's work: the
// culprit's data is emptied, not its row (the file is the user's), so
// delete runs the same handler, parameters, check and confirm as clear.
// Both names work (Keep actions); culprit.delete was clear's alias.
func (a *App) defineCoreDelete() {
	src := a.r.Operations[a.r.CanonicalKey("culprit.clear")]
	op := src
	op.Noun, op.Verb, op.Standard, op.Unavailable = "culprit", "delete", "", ""
	op.Aliases = nil
	op.Description = src.Description + " (as culprit.clear)"
	if op.App != nil {
		op.Run = nil
	}
	a.r.Register(op)
	a.r.Define("culprit").Verbs("delete")
}
