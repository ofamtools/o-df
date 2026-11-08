<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/logo-animated-dark.svg">
    <img alt="o-df" src="assets/logo-animated-light.svg" width="360">
  </picture>
  <br>
  <a href="https://github.com/ofamtools/o-df/tags"><img alt="Latest tag" src="https://img.shields.io/github/v/tag/ofamtools/o-df"></a>
  <a href="https://pkg.go.dev/ofam.dev/o-df"><img alt="Go Reference" src="https://pkg.go.dev/badge/ofam.dev/o-df.svg"></a>
  <a href="go.mod"><img alt="Go version" src="https://img.shields.io/github/go-mod/go-version/ofamtools/o-df"></a>
  <a href="LICENSE"><img alt="License" src="https://img.shields.io/github/license/ofamtools/o-df"></a>
</p>

<p align="center">Finds and clears the usual disk hogs on a Mac: package, browser and build caches, model downloads. Each culprit is a row of data.</p>

`o-df` (so `o df`) finds and clears the usual disk hogs on a Mac: package
caches, browser caches, build caches, model downloads, the Downloads folder.
Each culprit is a row of data, not code: o-df ships a default list and reads
yours from `~/.config/o-df/culprits.json`. The defaults name no volume and no
folder layout: where `relocate` copies to and where your projects live are
yours to set (see [Configuring culprits](#configuring-culprits)).

## Install

Install with the Go version in [go.mod](go.mod) or newer:

```sh
go install ofam.dev/oden/cmd/o@latest   # oden's o: `o df` runs o-df, no dash needed
go install ofam.dev/o-df@latest
mtm extension register df   # optional: the culprits page as an mtm tab
```

## Usage

```sh
o df                      # the same as o df scan
o df scan [--all] [--json] [--timeout 60s] [--external]
o df top [PATH] [--depth 2] [--n 30] [--min 100M] [--timeout 5m] [--external] [--json]
o df culprit list [--all] [--format table]
o df culprit show NAME
o df culprit get NAME --prop size
o df free NAME... [--yes]   # clear culprits; o df delete is the same command
o df free --all [--yes]     # clear every present rebuildable culprit
o df culprit clear NAME [--yes]            # culprit free and culprit delete too
o df culprit clear --names '["a","b"]' | --all [--yes]
o df culprit relocate NAME [--to DIR] [--link] [--same-volume] [--yes]
o df tui [--smoke] [--all]  # the culprits page
o df catalog              # the nouns, verbs and page for a host
```

## scan

`o df scan` prints the volume's totals and every culprit with at least one
path present, biggest first: name, size, rebuildable, when it was last used
(the newest access or change time inside it), the advice (delete, relocate
or keep) and its paths. A path that is a symlink shows as `path → target`
and is not counted: it already lives elsewhere. `--all` lists absent
culprits too; `--json` prints `{disk, total, culprits: [report...]}`.

Sizes are allocated bytes (`st_blocks`), as `du` counts them, so a sparse
disk image counts what it uses. A hard-linked file counts once. A size
marked `≥` met an unreadable directory (for example `~/.Trash` without Full
Disk Access) and is a lower bound. A path whose walk takes longer than
`--timeout` (default 60s; a directory another process holds in an
uninterruptible wait never answers) shows `timeout` instead of hanging the
scan.

### Other volumes

o-df reads only the home directory's volume. The walk never crosses into
another volume or follows a symlink, and a culprit path that resolves
(through a symlinked directory above it) onto another volume shows as
`skipped` and is neither sized nor cleared. To read one anyway, set
`"external": true` on the row, or at the top of the culprits file for every
row and `top`, or pass `--external` to `scan`, `top` or `culprit list`.

## top

`o df top PATH` walks PATH once, in parallel, without crossing into another
volume or following symlinks, and prints its biggest directories (down to
`--depth` levels) and files of at least `--min`, biggest first. Use it to
find what no culprit row covers yet. A PATH on another volume is refused
without `--external`; after `--timeout` (default 5m) it prints the
directories it finished, marked partial:

```sh
o df top ~ --depth 2 --min 1G
o df top ~/Downloads --depth 1 --min 500M
```

## culprit verbs

`culprit` is a noun with six verbs, the same on the command line, in the
TUI and from a host (`o df catalog`). Every one prints JSON
(`--format table|markdown` for the list).

A NAME may be abbreviated, as oden matches command words: an exact name or
alias wins, else a unique prefix (`o df free npm` is npm-cache, `o df
culprit show go-b` is go-build-cache, `o df free brew` is homebrew-cache by
its alias). A prefix of several fails listing them (`"go" is ambiguous:
go-build-cache, go-mod-cache`), an unknown name lists the closest, and
clear, free, delete and relocate never act on either; an abbreviated name's
confirm names the full culprit.

- `list`: rows `{noun, id: culprit/NAME, label, data: report, cells}`.
- `names [--query WORD]`: the names WORD may mean, `[{id, label}]` (every
  name for no word): the palette's choices for NAME.
- `show NAME`: the row and every path it expands to, sized.
- `get NAME --prop size`: `{name, bytes, human, files, last_used}` (the computed size prop of a culprit).
- `clear NAME`: only for a rebuildable culprit, after a confirm (`--yes`
  answers it). It runs the row's `clear` commands, or removes the contents
  of each present path (the directories stay). Read-only trees are made
  writable first. Prints `{name, before, after, freed}`. `free` and `delete`
  are other names of it (`o df culprit free NAME`).
- `clear --names '["a","b"]'` or `clear --all`: several culprits after one
  confirm that lists each with its size and the total. `--all` takes every
  present rebuildable culprit; a culprit that is not rebuildable (Downloads,
  Movies, Trash, Docker, model folders) is never included, and the confirm
  names the ones it leaves out. Naming one that is not rebuildable refuses
  the whole request. Prints `{cleared: [{name, before, after, freed}...],
  skipped, freed}`.
- `relocate NAME [--to DIR] [--link]`: copies each present path to
  `DIR/<path under home>` (DIR: `--to`, else `relocate_to` in your culprits
  file; with neither it fails and says so, it never guesses), compares the copy
  with the source entry by entry (kind, size, symlink target), deletes the
  source and, with `--link`, leaves a symlink to the copy. It refuses a
  destination that exists or that is on the source's volume (that frees
  nothing; `--same-volume` allows it anyway). Prints
  `{name, dir, moved: [{from, to, bytes, linked}], freed}`.

Both changes refuse `/`, the home directory and anything above it.

## free and delete

`o df free` (or `o df delete`, the same command) is `culprit clear` at the
top level, for several names at once:

```sh
o df free npm-cache pnpm-store   # one confirm for both
o df free --all                  # every present rebuildable culprit
o df delete --all --yes          # without asking
```

```
Clear 3 culprits, 860K in all? Their tools rebuild them on demand.
  pnpm-store     492K
  npm-cache      296K
  turbo-cache     72K
Not rebuildable, never included: downloads 880K. [y/N]
```

## The TUI

`o df tui` opens the culprits page: the list on the left, the focused
culprit's detail (every path, its size, the rebuild hint, the clear
commands) on the right, and a status line with the disk's totals and the
size of the marked rows.

| Key | Does |
|---|---|
| `j` / `k` | move |
| space | mark or unmark a row |
| `d d` | clear the marked rows (the focused one when none is marked), one confirm for all |
| `D` | clear every present rebuildable culprit, one confirm listing them (`culprit.clear {all: true}`) |
| `m` | relocate the marked rows to `relocate_to`, one confirm for all |
| `R` | size everything again |
| `?` | every key |
| `q` | quit |

The keys are binding rows (`list.apply {verb: clear}`, `culprit.clear {all:
true}`, `list.apply {verb: relocate}`), so a `bindings.json` row can add a
key for another destination (`list.apply {verb: relocate, args: {to:
"/Volumes/Backup/relocated"}}`) or unbind `D` without code.

## Configuring culprits

The defaults are in [culprits.json](culprits.json), embedded in the binary.
Your file (`~/.config/o-df/culprits.json`, or `$XDG_CONFIG_HOME/o-df/`, or
the path in `$O_DF_CULPRITS`) has the same shape. A row with a new name is
added; a row with a default's name replaces it whole; `"disabled": true`
drops it. Unknown fields are an error, so a typo does not pass silently.

```json
{
  "relocate_to": "/Volumes/Backup/relocated",
  "project_roots": ["~/src", "~/code"],
  "culprits": [
    {"name": "node-modules", "title": "node_modules in projects", "category": "js",
     "paths": ["{project_root}/*/node_modules"], "rebuildable": true,
     "rebuild": "npm install", "recommend": "delete"},
    {"name": "sorbet-cache", "title": "Sorbet caches", "category": "ruby",
     "paths": ["{project_root}/*/sorbet/.cache"], "rebuildable": true,
     "rebuild": "srb tc rebuilds it", "recommend": "delete"},
    {"name": "docker-build-cache", "paths": ["~/Library/Containers/com.docker.docker"],
     "rebuildable": true, "size": ["sh", "-c", "docker system df --format '{{.Size}}' | head -1"],
     "clear": [["docker", "builder", "prune", "--all", "--force"]], "recommend": "delete"},
    {"name": "trash", "disabled": true, "paths": ["~/.Trash"]}
  ]
}
```

Two top-level settings say where things are on your machine, and a third
lets the walk reach other volumes. The defaults set none of them:

| Setting | Default | Meaning |
|---|---|---|
| `relocate_to` | none | where `relocate` (and the TUI's `m`) copies to when `--to` is not given; with neither, relocate fails before asking |
| `project_roots` | none | the directories that hold your projects; a culprit path holding `{project_root}` (the default `turbo-cache` row, your own `node_modules` or `target` rows) expands once per root, and finds nothing while the list is empty |
| `external` | false | allow paths on other volumes for every row and `top` |

A culprit row's fields:

| Field | Meaning |
|---|---|
| `name` | one word, the id (`culprit/NAME`) |
| `aliases` | more words that name the row (`brew` for homebrew-cache) |
| `title`, `category`, `notes` | shown in the detail |
| `paths` | `~`, `$HOME`, `*` (one path segment, `filepath.Glob`) and `{project_root}` (each of `project_roots`) and `{tmp}` (the system scratch directory); a missing path is absent |
| `rebuildable` | the owning tool recreates the data; only these can be cleared |
| `rebuild` | how it comes back |
| `paths_from` | a command whose stdout lines are more paths, added to `paths` and listed once each (`["go","env","GOCACHE"]`); a missing or failing command adds none |
| `min_age` | `24h`, `7d`: a matched path whose newest file is younger is `young`, neither sized nor cleared |
| `skip`, `skip_with` | base-name globs to leave out; names (`.git`) that, within three levels below a matched directory, leave it out |
| `remove_matches` | clear deletes each matched path itself, not only its contents |
| `size` | a command whose stdout's first integer is the size in bytes, instead of walking `paths` |
| `clear` | commands (each an argv) instead of removing the paths' contents; a command holding `{path}` runs once per present path |
| `recommend` | `delete`, `relocate` or `keep`: the advice the scan prints |
| `external` | allow paths that resolve onto another volume (top level: every row and `top`) |
| `disabled` | drop a default row |

## Build

```sh
GOWORK=off GOFLAGS=-trimpath go build -o o-df .
gofmt -l . && GOWORK=off go vet ./... && GOWORK=off go test ./...
GOWORK=off go run . tui --smoke
```

Tests build a temp HOME fixture and never touch the real one.
