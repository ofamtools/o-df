package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"ofam.dev/ochazuke/core"
	otea "ofam.dev/ochazuke/tea"
)

// driver feeds a Shell synchronously without a TTY: every command a step
// returns runs at once and its message is fed back. The smoke run and
// tests use it; it expects PrefixTimeout 0 and MaxFPS 0.
type driver struct{ sh *otea.Shell }

func newDriver(sh *otea.Shell, initCmd tea.Cmd, w, h int) *driver {
	sh.Engine.PrefixTimeout = 0
	sh.MaxFPS = 0
	d := &driver{sh: sh}
	d.send(tea.WindowSizeMsg{Width: w, Height: h})
	d.pump(initCmd)
	return d
}

func (d *driver) send(msg tea.Msg) {
	_, cmd := d.sh.Update(msg)
	d.pump(cmd)
}

func (d *driver) pump(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	var m tea.Msg
	select {
	case m = <-done:
	case <-time.After(5 * time.Second):
		return
	}
	switch m := m.(type) {
	case tea.BatchMsg:
		for _, c := range m {
			d.pump(c)
		}
	case tea.QuitMsg, nil:
	default:
		d.send(m)
	}
}

// keys sends key presses by core name: "j", "G", "ctrl+d", "enter", "esc",
// "space", "backspace", "tab", "up", "down", "left", "right".
func (d *driver) keys(names ...string) {
	for _, n := range names {
		d.send(keyMsg(n))
	}
}

// typeText sends each rune of s as a key press.
func (d *driver) typeText(s string) {
	for _, r := range s {
		if r == ' ' {
			d.send(keyMsg("space"))
		} else {
			d.send(tea.KeyPressMsg{Code: r, Text: string(r)})
		}
	}
}

// command dispatches a command directly as the palette or a script would.
func (d *driver) command(id string) {
	d.send(core.CommandEvent{Command: core.Command{ID: id, Source: "palette", Actor: core.Actor{Kind: "user"}}})
}

var namedKeys = map[string]rune{
	"enter": tea.KeyEnter, "esc": tea.KeyEscape, "tab": tea.KeyTab, "backspace": tea.KeyBackspace,
	"up": tea.KeyUp, "down": tea.KeyDown, "left": tea.KeyLeft, "right": tea.KeyRight,
	"pgup": tea.KeyPgUp, "pgdown": tea.KeyPgDown, "home": tea.KeyHome, "end": tea.KeyEnd,
	"delete": tea.KeyDelete,
}

func keyMsg(n string) tea.KeyPressMsg {
	var mod tea.KeyMod
	for {
		if rest, ok := strings.CutPrefix(n, "ctrl+"); ok && len(rest) > 0 {
			mod, n = mod|tea.ModCtrl, rest
		} else if rest, ok := strings.CutPrefix(n, "alt+"); ok && len(rest) > 0 {
			mod, n = mod|tea.ModAlt, rest
		} else if rest, ok := strings.CutPrefix(n, "shift+"); ok && len(rest) > 0 {
			mod, n = mod|tea.ModShift, rest
		} else {
			break
		}
	}
	if r, ok := namedKeys[n]; ok {
		return tea.KeyPressMsg{Code: r, Mod: mod}
	}
	if n == "space" {
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " ", Mod: mod}
	}
	r := []rune(n)[0]
	if mod != 0 {
		return tea.KeyPressMsg{Code: r, Mod: mod}
	}
	return tea.KeyPressMsg{Code: r, Text: n}
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;:?]*[A-Za-z]|\x1b\][^\a\x1b]*(\a|\x1b\\)`)

func plain(s string) string { return ansiRe.ReplaceAllString(s, "") }

func maxLineW(s string) int {
	w := 0
	for _, l := range strings.Split(s, "\n") {
		w = max(w, textW(l))
	}
	return w
}

// frameOnce sizes the culprits, renders one frame to w and exits (o-df tui --smoke).
func frameOnce(a *App, cfg UIConfig, width, height int, out io.Writer) error {
	sh, initCmd := newShell(newUI(a, cfg))
	newDriver(sh, initCmd, width, height)
	f := plain(sh.View().Content)
	_, err := fmt.Fprintln(out, strings.TrimRight(f, "\n"))
	return err
}

// smokeEnv builds a self-contained temp fixture directory for --smoke;
// nothing touches the real home directory or any real cache.
func smokeEnv() (Env, func(), error) {
	home, err := os.MkdirTemp("", "o-df-smoke-*")
	if err != nil {
		return Env{}, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(home) }
	write := func(rel string, size int, ageDays int) error {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, bytes.Repeat([]byte("x"), size), 0o644); err != nil {
			return err
		}
		if ageDays > 0 {
			mt := time.Now().Add(-time.Duration(ageDays) * 24 * time.Hour)
			_ = os.Chtimes(p, mt, mt)
		}
		return nil
	}
	for _, f := range []struct {
		rel     string
		size    int
		ageDays int
	}{
		{".npm/_cacache/a", 64 << 10, 0},
		{".npm/_npx/x/package.json", 4 << 10, 0},
		{"Library/pnpm/store/v10/f", 128 << 10, 0},
		{"Downloads/big.zip", 256 << 10, 0},
		{"src/one/.turbo/cache/blob", 16 << 10, 0},
		{"bk/a.bak-1", 4 << 10, 1},
		{"bk/a.bak-9", 8 << 10, 9},
	} {
		if err := write(f.rel, f.size, f.ageDays); err != nil {
			cleanup()
			return Env{}, nil, err
		}
	}
	userFile := filepath.Join(home, ".config", "o-df", "culprits.json")
	if err := os.MkdirAll(filepath.Dir(userFile), 0o755); err != nil {
		cleanup()
		return Env{}, nil, err
	}
	cfgJSON := `{"project_roots": ["~/src"], "culprits": [
		{"name": "copies", "title": "Backup copies", "category": "backups", "paths": ["~/bk/*"], "rebuildable": false, "days": 5, "recommend": "keep"}
	]}`
	if err := os.WriteFile(userFile, []byte(cfgJSON), 0o644); err != nil {
		cleanup()
		return Env{}, nil, err
	}
	env := Env{
		Home:     home,
		UserFile: userFile,
		Run:      func(context.Context, []string) ([]byte, error) { return nil, nil },
		Remove:   os.RemoveAll,
	}
	return env, cleanup, nil
}

// runSmoke drives a scripted 120x32 session over temp fixture dirs through
// the real keymap, engine, shell and renderer, prints the frame, checks
// and summary, and cleans up the temp dir.
func runSmoke(stdout io.Writer) int {
	env, cleanup, err := smokeEnv()
	if err != nil {
		fmt.Fprintln(stdout, "smoke:", err)
		return 1
	}
	defer cleanup()
	app, err := New(env)
	if err != nil {
		fmt.Fprintln(stdout, "smoke:", err)
		return 1
	}
	u := newUI(app, UIConfig{})
	sh, initCmd := newShell(u)
	d := newDriver(sh, initCmd, 120, 32)
	page := func() Page { return pageOf(sh.State) }
	focused := func() string {
		p := page()
		id, _ := p.focusedName()
		return id
	}
	frame := func() string { return plain(sh.View().Content) }

	var results []string
	fails := 0
	check := func(name string, ok bool) {
		mark := "ok  "
		if !ok {
			mark, fails = "FAIL", fails+1
		}
		results = append(results, mark+" "+name)
	}

	f := frame()
	check("frame is 120x32", len(strings.Split(f, "\n")) == 32 && maxLineW(f) <= 120)
	check("initial async scan loaded culprits", page().Loaded && len(page().Reports) == 5)
	check("detail pane shows focused culprit", page().Detail && strings.Contains(f, "Detail") && strings.Contains(f, "downloads"))

	d.keys("j")
	check("j moves to second culprit (pnpm-store)", focused() == "pnpm-store")

	d.keys("/")
	d.typeText("npm")
	check("/ filters while typing", page().Typing && len(page().Grid.Rows) == 2)
	d.keys("ctrl+a", "ctrl+k")
	check("readline ctrl+a ctrl+k clears query", page().Query == "" && len(page().Grid.Rows) == 5)
	d.typeText("pnpm")
	d.keys("enter")
	check("enter keeps filter in normal mode", !page().Typing && page().Query == "pnpm" && len(page().Grid.Rows) == 1)
	d.keys("esc")
	check("esc clears filter", page().Query == "" && len(page().Grid.Rows) == 5)

	d.keys("s")
	check("s cycles sort to name", page().Sort == "name")
	d.keys("S")
	check("S reverses sort", page().Desc)
	d.keys("g", "b")
	check("g b groups by category", page().Grid.GroupField == "category")
	d.keys("g", "b", "g", "b", "g", "b")
	check("g b cycles back to ungrouped", page().Grid.GroupField == "")

	// Dry-run clean confirm + cancel (changes nothing).
	d.keys("g", "g")
	for i := 0; i < len(page().Grid.Rows); i++ {
		if focused() == "npm-cache" {
			break
		}
		d.keys("j")
	}
	d.keys("d", "d")
	check("d d opens clean dry-run confirm", len(sh.State.Modals) == 1 && sh.State.Modals[0].ID == modalConfirm)
	d.keys("n")
	_, npmErr := os.Stat(filepath.Join(env.Home, ".npm/_cacache/a"))
	check("n cancels clean without deleting", len(sh.State.Modals) == 0 && npmErr == nil)

	// Dry-run trim preview on copies (removes nothing).
	d.keys("g", "g")
	for i := 0; i < len(page().Grid.Rows); i++ {
		if focused() == "copies" {
			break
		}
		d.keys("j")
	}
	d.keys("t")
	check("t opens trim dry-run preview", len(sh.State.Modals) == 1 && sh.State.Modals[0].ID == modalPreview && strings.Contains(frame(), "a.bak-9"))
	d.keys("esc")
	_, bakErr := os.Stat(filepath.Join(env.Home, "bk/a.bak-9"))
	check("trim dry-run left files intact", len(sh.State.Modals) == 0 && bakErr == nil)

	// Theme & density cycle.
	d.keys("g", "t")
	check("g t cycles theme", page().ThemeIdx == 1 && sh.Theme.Name == u.Themes[1].Name)
	d.keys("g", "d")
	check("g d cycles density", page().Density == "compact" && sh.Theme.Density == "compact")
	d.keys("g", "t", "g", "t", "g", "d", "g", "d")

	// Palette & help.
	d.keys(":")
	check(": opens command palette", len(sh.State.Modals) == 1 && sh.State.Modals[0].ID == modalPalette)
	d.keys("esc", "?")
	check("? opens live help", len(sh.State.Modals) == 1 && strings.Contains(frame(), "culprit.clean"))
	d.keys("esc")

	// Geometry check.
	p := page()
	g, geo := sh.State.Layout.Rects[nodeGrid], u.geometry(&p, 120, 32)
	check("grid geometry matches layout", g.W == geo.tableW && g.H == geo.tableH)

	f = frame()
	for _, line := range strings.Split(f, "\n") {
		fmt.Fprintln(stdout, strings.TrimRight(line, " "))
	}
	fmt.Fprintln(stdout)
	for _, r := range results {
		fmt.Fprintln(stdout, r)
	}
	fmt.Fprintf(stdout, "smoke: %d/%d checks passed · %d culprits · %d renders\n", len(results)-fails, len(results), len(page().Reports), sh.Renders)
	if fails > 0 {
		return 1
	}
	return 0
}
