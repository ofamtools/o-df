package main

import (
	"strings"
	"time"

	"ofam.dev/ochazuke/core"
	"ofam.dev/oden"
)

// The culprit noun on the catalog protocol (matomeru-next/ext/catalog.go):
//
//	o-df catalog          a manifest: the df.culprit schema and o-df's commands
//	o-df query [TEXT]     {"items": [record...], "total": n}
//
// The culprits file is the store and it is read only: a host shadows what
// the owner does to a record and never writes here. Clear, trim and
// relocate are commands, never record writes.

// CulpritKind is the record kind of a culprit.
const CulpritKind = Namespace + ".culprit"

// Namespace is the singular namespace o-df declares: its commands are
// df.<noun>.<verb> and a host mounts its catalog under it.
const Namespace = "df"

// Schema is the wire form of a host's data.Schema.
type Schema = oden.ExtSchema

// Manifest is what `o-df catalog` prints (ext.Manifest's wire form).
type Manifest = oden.ExtManifest

// Record is the wire form of a host's data.Record.
type Record struct {
	Kind            string         `json:"kind"`
	ID              string         `json:"id"`
	Fields          map[string]any `json:"fields"`
	ArchiveStatus   string         `json:"archiveStatus"`
	StarStatus      string         `json:"starStatus"`
	ChangeSetNumber int64          `json:"changeSetNumber"`
	CreatedAt       time.Time      `json:"createdAt"`
	UpdatedAt       time.Time      `json:"updatedAt"`
}

// QueryResult is what `o-df query` prints.
type QueryResult struct {
	Items []Record `json:"items"`
	Total int      `json:"total"`
}

func culpritFields() []oden.ExtField {
	text := func(name, desc string) oden.ExtField {
		return oden.ExtField{Name: name, Type: string(core.ParamString), Description: desc}
	}
	return []oden.ExtField{
		{Name: "name", Type: string(core.ParamString), Required: true, Description: "The culprit's name (an alias or a unique prefix names it too)"},
		text("title", "What the row calls itself"),
		text("size", `Human-readable disk size (e.g. "2.9 GB", "547 MB")`),
		{Name: "sizeBytes", Type: string(core.ParamInt), Description: "Disk size in bytes"},
		{Name: "rebuildable", Type: string(core.ParamBool), Description: "Whether the owning tool recreates the data on demand (only such a row can be cleaned)"},
		text("category", "The kind of cache or data"),
		{Name: "recommend", Type: string(core.ParamString), Enum: []string{"delete", "relocate", "keep"}, Description: "The advice the scan prints"},
		{Name: "aliases", Type: string(core.ParamList), Description: "More words that name the row"},
		{Name: "paths", Type: string(core.ParamList), Description: "The paths the row sizes (~-relative or absolute)"},
		text("rebuild", "How the data comes back"),
		text("notes", "Notes on the row"),
		{Name: "trimmable", Type: string(core.ParamBool), Description: "Whether the row has a retention rule (keep, days) trim applies"},
		text("source", "Where the row came from: default or the user file"),
		{Name: "bytes", Type: string(core.ParamInt), Description: "Size in bytes (alias of sizeBytes)"},
		text("human", "Compact size, df -h style"),
		{Name: "files", Type: string(core.ParamInt), Description: "Files counted"},
		{Name: "present", Type: string(core.ParamBool), Description: "Whether any path exists"},
		text("last_used", "Newest modification, RFC 3339"),
	}
}

func culpritSchema() oden.ExtSchema {
	return oden.ExtSchema{
		Kind: CulpritKind, Title: "Culprit", Plural: "Culprits",
		Fields:         culpritFields(),
		DefaultColumns: []string{"name", "size", "rebuildable", "category", "recommend"},
		SummaryField:   "title",
		UniqueFields:   []string{"name"},
		StyleRules: []oden.ExtStyleRule{
			{When: `fields.rebuildable == true && fields.recommend == "delete"`, Target: "recommend", Tone: "success"},
			{When: `fields.rebuildable == false`, Target: "rebuildable", Tone: "muted"},
		},
	}
}

// commandSpecsWire are o-df's commands as a host would run them (o-df ARGS).
func commandSpecsWire() []oden.ExtCommand {
	name := oden.ExtField{Name: "name", Type: string(core.ParamRef), RefKinds: []string{CulpritKind}, Required: true, Description: "A culprit name, an alias or a unique prefix of one"}
	cmd := func(verb, desc string, params ...oden.ExtField) oden.ExtCommand {
		return oden.ExtCommand{
			ID:          CulpritKind + "." + verb,
			Description: desc,
			Params:      params,
			Tags:        []string{"o-df"},
			Meta:        map[string]any{"argv": []string{"o-df", verb}},
		}
	}
	boolp := func(n, d string) oden.ExtField {
		return oden.ExtField{Name: n, Type: string(core.ParamBool), Description: d}
	}
	strp := func(n, d string) oden.ExtField {
		return oden.ExtField{Name: n, Type: string(core.ParamString), Description: d}
	}
	clean := oden.ExtCommand{
		ID:          "df.culprit.clean",
		Description: "Free disk space for a rebuildable culprit via o-df clean --yes",
		Params:      []oden.ExtField{name, boolp("all", "every present rebuildable culprit"), boolp("yes", "do not ask")},
		Kinds:       []string{CulpritKind},
		Tags:        []string{"o-df", "editing", "destructive"},
		Key:         "X",
		KeyHint:     "clean culprit",
		Exec: &oden.ExtCommandExec{
			Argv:         []string{"{{binary}}", "clean", "{{name|title|id}}", "--yes"},
			Title:        "o-df clean {{name|title|id}}",
			RefreshKinds: []string{CulpritKind},
		},
		Guard: &oden.ExtCommandGuard{
			When:          "fields.rebuildable == true",
			RefuseMessage: "Cannot clean {{name|title|id}}: culprit is not rebuildable",
			HideWhenFalse: false,
		},
		Confirm: &oden.ExtCommandConfirm{
			Title:   "Clean {{name|title|id}}",
			Message: "Free {{sizeHuman|size|'space'}} from {{name|title|id}} via o-df clean --yes?",
			Label:   "Clean",
			Tone:    "warning",
		},
		Feedback: &oden.ExtCommandFeedback{
			Running: "Cleaning {{name|title|id}}...",
			Success: "Freed {{sizeHuman|size|'space'}} from {{name|title|id}}",
			Failure: "o-df clean {{name|title|id}} failed",
		},
		Meta: map[string]any{"argv": []string{"o-df", "clean"}, "destructive": true, "confirm": true},
	}
	relocate := cmd("relocate", "Copy a culprit's paths to TO/<path under home>, verify, delete the source; asks first", name, oden.ExtField{Name: "to", Type: string(core.ParamPath), Description: "destination root (default relocate_to)"}, boolp("link", "leave a symlink"), boolp("yes", "do not ask"))
	relocate.Meta["destructive"] = true
	relocate.Meta["confirm"] = true
	return []oden.ExtCommand{
		cmd("scan", "Size every culprit and read the disk's totals: {disk, total, culprits} (read-only)", boolp("all", "include absent culprits"), boolp("external", "size paths on other volumes")),
		cmd("top", "The biggest directories and files under PATH (read-only)", oden.ExtField{Name: "path", Type: string(core.ParamPath)}, oden.ExtField{Name: "depth", Type: string(core.ParamInt), Default: 2}, strp("min", "smallest size reported (default 100M)")),
		cmd("show", "One culprit's row and every path it expands to, sized (read-only)", name),
		cmd("culprits", "Every culprit row of the merged culprits file, unsized (read-only)"),
		clean,
		cmd("trim", "Remove what a row's retention rule (keep, days, group, min_keep) says; a dry run unless --apply", name, boolp("apply", "remove the files (asks first)"), boolp("yes", "do not ask")),
		relocate,
	}
}

func catalogManifest() oden.ExtManifest {
	return oden.ExtManifest{Name: "o-df", Version: Version, Schemas: []oden.ExtSchema{culpritSchema()}, Commands: commandSpecsWire()}
}

func recordSizeText(r Report) string {
	size := HumanSize(r.Bytes)
	if r.Errors > 0 {
		size = "≥" + size
	}
	for _, f := range r.Found {
		if f.Timeout {
			size = "timeout"
		}
	}
	return size
}

// culpritRecord is a culprit row as a record; rep, when set, adds its size.
func culpritRecord(c Culprit, rep *Report) Record {
	f := map[string]any{
		"name":        c.Name,
		"title":       c.Title,
		"category":    c.Category,
		"aliases":     nonNil(c.Aliases),
		"paths":       nonNil(c.Paths),
		"rebuildable": c.Rebuildable,
		"rebuild":     c.Rebuild,
		"recommend":   c.Recommend,
		"notes":       c.Notes,
		"trimmable":   c.Trimmable(),
		"source":      c.Source,
	}
	if f["title"] == "" {
		f["title"] = c.Name
	}
	if rep != nil {
		f["size"] = recordSizeText(*rep)
		f["sizeBytes"] = rep.Bytes
		f["bytes"], f["human"], f["files"], f["present"] = rep.Bytes, Human(rep.Bytes), rep.Files, rep.Present
		if !rep.LastUsed.IsZero() {
			f["last_used"] = rep.LastUsed.UTC().Format(time.RFC3339)
		}
	}
	return Record{Kind: CulpritKind, ID: c.Name, Fields: f, ArchiveStatus: "active", StarStatus: "unstarred"}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// matchesQuery is a case-insensitive substring match over a culprit's
// name, aliases, title, category and notes.
func matchesQuery(c Culprit, q string) bool {
	if q == "" {
		return true
	}
	q = strings.ToLower(q)
	for _, s := range append([]string{c.Name, c.Title, c.Category, c.Notes}, c.Aliases...) {
		if strings.Contains(strings.ToLower(s), q) {
			return true
		}
	}
	return false
}
