package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestParseSetFlags(t *testing.T) {
	t.Run("trims whitespace around the equals sign", func(t *testing.T) {
		edits, err := parseSetFlags([]string{"title = My Song", " artist=The Band "})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if edits["title"] != "My Song" {
			t.Errorf("title = %q, want %q", edits["title"], "My Song")
		}
		if edits["artist"] != "The Band" {
			t.Errorf("artist = %q, want %q", edits["artist"], "The Band")
		}
	})

	t.Run("keeps interior whitespace and empty values", func(t *testing.T) {
		edits, err := parseSetFlags([]string{"album=A B C", "genre="})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if edits["album"] != "A B C" {
			t.Errorf("album = %q, want %q", edits["album"], "A B C")
		}
		if v, ok := edits["genre"]; !ok || v != "" {
			t.Errorf("genre = %q (present %v), want empty clear", v, ok)
		}
	})

	t.Run("rejects bad and duplicate flags", func(t *testing.T) {
		for _, bad := range [][]string{
			nil,                    // none
			{"noequals"},           // missing '='
			{"=value"},             // empty field
			{"title=a", "title=b"}, // duplicate
		} {
			if _, err := parseSetFlags(bad); err == nil {
				t.Errorf("parseSetFlags(%q): want error, got nil", bad)
			}
		}
	})
}

func TestLoadBatchEdits(t *testing.T) {
	doc := `[
		{"itemPid": "i1", "fields": {"title": "Opener", "track_no": "1"}},
		{"itemPid": "i2", "fields": {"title": "Closer"}}
	]`

	t.Run("reads a file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "batch.json")
		if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
		edits, err := loadBatchEdits(&cobra.Command{}, path)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(edits) != 2 || edits[0].ItemPID != "i1" || edits[0].Fields["track_no"] != "1" ||
			edits[1].Fields["title"] != "Closer" {
			t.Fatalf("edits = %+v", edits)
		}
	})

	t.Run("reads stdin via dash", func(t *testing.T) {
		cmd := &cobra.Command{}
		cmd.SetIn(strings.NewReader(doc))
		edits, err := loadBatchEdits(cmd, "-")
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(edits) != 2 {
			t.Fatalf("edits = %+v", edits)
		}
	})

	t.Run("rejects malformed documents", func(t *testing.T) {
		for name, bad := range map[string]string{
			"empty array": `[]`,
			"no pid":      `[{"fields": {"title": "x"}}]`,
			"no fields":   `[{"itemPid": "i1"}]`,
			"not json":    `{`,
			// The engine refuses a repeated item unconditionally, so the loader
			// refuses it too instead of letting a dry-run preview both entries.
			"duplicate pid": `[{"itemPid": "i1", "fields": {"title": "a"}},
				{"itemPid": "i1", "fields": {"title": "b"}}]`,
		} {
			cmd := &cobra.Command{}
			cmd.SetIn(strings.NewReader(bad))
			if _, err := loadBatchEdits(cmd, "-"); err == nil {
				t.Errorf("%s: want error, got nil", name)
			}
		}
	})
}
