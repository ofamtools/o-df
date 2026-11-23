package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var nonDomainFilenamePattern = regexp.MustCompile(`(?i)(^|_)(wave|sprint|phase|milestone)[a-z0-9]*(_|\.go$)`)

const maxProductionFileLines = 1000

// legacyLineCeilings freezes existing production files above maxProductionFileLines
// at their exact current line counts so they cannot grow and no new god-files enter.
var legacyLineCeilings = map[string]int{
	"page.go": 1260,
}

func walkGoFiles(t *testing.T, fn func(relPath, base string)) {
	t.Helper()
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".git" || name == ".worktrees" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		fn(filepath.ToSlash(path), name)
		return nil
	})
	if err != nil {
		t.Fatalf("walk Go files: %v", err)
	}
}

func TestDomainFilenames(t *testing.T) {
	walkGoFiles(t, func(relPath, base string) {
		if nonDomainFilenamePattern.MatchString(base) {
			t.Errorf("%s: filename contains sprint/wave/phase/milestone marker; use a domain-specific name", relPath)
		}
	})
}

func TestNoGodFiles(t *testing.T) {
	walkGoFiles(t, func(relPath, base string) {
		if strings.HasSuffix(base, "_test.go") {
			return
		}
		data, err := os.ReadFile(relPath)
		if err != nil {
			t.Fatalf("read %s: %v", relPath, err)
		}
		lines := bytes.Count(data, []byte{'\n'})
		if len(data) > 0 && data[len(data)-1] != '\n' {
			lines++
		}
		limit := maxProductionFileLines
		if ceiling, ok := legacyLineCeilings[relPath]; ok {
			limit = ceiling
		}
		if lines > limit {
			t.Errorf("%s has %d lines (limit %d); split by domain responsibility", relPath, lines, limit)
		}
	})
}
