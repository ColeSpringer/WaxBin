package sqlite

import (
	"context"
	"slices"
	"testing"
)

// TestSchemaBatchColumnsExist: the columns and indexes later work writes and seeks are in
// the baseline, so none can be found missing when it is needed. A virtual generated
// column is hidden from pragma_table_info, hence the xinfo forms.
func TestSchemaBatchColumnsExist(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	columns := map[string][]string{
		"file":            {"name"},
		"playable_item":   {"title_sort"},
		"artist":          {"name_sort"},
		"album":           {"title_sort"},
		"release_group":   {"title_sort"},
		"entity_fold":     {"name", "folder", "library_id"},
		"track":           {"album_sort", "album_artist_sort"},
		"rollup_dirty":    {"kind", "entity_id"},
		"playlist":        {"description"},
		"file_diagnostic": {"source", "provider"},
		"job":             {"error_code", "error_reason"},
		"trash_pending": {"pid", "file_id", "item_pid", "item_kind", "library_id", "orig_path",
			"orig_display", "essence_hash", "size", "trash_path", "trash_display", "reason", "job_pid",
			"created_at"},
	}
	for table, want := range columns {
		var have []string
		rows, err := st.rdb().QueryContext(ctx, "SELECT name FROM pragma_table_xinfo(?)", table)
		if err != nil {
			t.Fatalf("%s columns: %v", table, err)
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatalf("%s columns: %v", table, err)
			}
			have = append(have, name)
		}
		rows.Close()
		for _, c := range want {
			if !slices.Contains(have, c) {
				t.Errorf("%s has no column %s (columns %v)", table, c, have)
			}
		}
	}

	type key struct{ col, coll string }
	indexes := map[string][]key{
		"file_essence":        {{"essence_hash", "BINARY"}, {"library_id", "BINARY"}},
		"file_name_nocase":    {{"name", "NOCASE"}},
		"file_display_nocase": {{"display_path", "NOCASE"}},
		"chapter_book_file":   {{"book_item_id", "BINARY"}, {"file_id", "BINARY"}},
	}
	for index, want := range indexes {
		var have []key
		rows, err := st.rdb().QueryContext(ctx,
			"SELECT name, coll FROM pragma_index_xinfo(?) WHERE key = 1 ORDER BY seqno", index)
		if err != nil {
			t.Fatalf("%s keys: %v", index, err)
		}
		for rows.Next() {
			var k key
			if err := rows.Scan(&k.col, &k.coll); err != nil {
				t.Fatalf("%s keys: %v", index, err)
			}
			have = append(have, k)
		}
		rows.Close()
		if !slices.Equal(have, want) {
			t.Errorf("index %s keys %v, want %v", index, have, want)
		}
	}
}

// TestFileNameIsTheLastPathSegment: file.name is what follows the last separator of the
// display path, either separator, and the whole path when it has none.
func TestFileNameIsTheLastPathSegment(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	res := putTrack(t, st, lib.ID, trackSpec{path: "/lib/A/b.flac", essence: "e1", content: "c1", title: "B"})
	cases := []struct{ display, want string }{
		{"/lib/A/b.flac", "b.flac"},
		{`C:\lib\Ä\ü é.mp3`, "ü é.mp3"},
		{`/lib/mixed\sep/x.ogg`, "x.ogg"},
		{`D:\lib/odd\y.opus`, "y.opus"},
		{"bare.wav", "bare.wav"},
		{"/lib/folder/", ""},
	}
	for _, c := range cases {
		if _, err := st.wdb().ExecContext(ctx, "UPDATE file SET display_path = ? WHERE pid = ?",
			c.display, string(res.FilePID)); err != nil {
			t.Fatalf("set display path: %v", err)
		}
		var got string
		if err := st.rdb().QueryRowContext(ctx, "SELECT name FROM file WHERE pid = ?",
			string(res.FilePID)).Scan(&got); err != nil {
			t.Fatalf("read name: %v", err)
		}
		if got != c.want {
			t.Errorf("name of %q = %q, want %q", c.display, got, c.want)
		}
	}
}
