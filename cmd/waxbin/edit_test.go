package main

import "testing"

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
