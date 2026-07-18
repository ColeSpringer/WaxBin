package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// queryPlan returns the EXPLAIN QUERY PLAN text for a statement, one node per line.
func queryPlan(t *testing.T, st *Store, stmt string, args ...any) string {
	t.Helper()
	rows, err := st.read.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+stmt, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var plan strings.Builder
	for rows.Next() {
		cells := make([]sql.NullString, len(cols))
		dest := make([]any, len(cols))
		for i := range cells {
			dest[i] = &cells[i]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		for _, c := range cells {
			if c.Valid {
				plan.WriteString(c.String)
				plan.WriteByte(' ')
			}
		}
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	return plan.String()
}

// TestTagIndexUsage confirms the item_tag_key index serves the catalog-wide, key-driven
// reads as a covering (index-only) scan (the facet and TagKeys) and also covers the
// per-item tag.<KEY> EXISTS predicate as an index seek, so no tag read falls back to a
// full table scan of item_tag.
func TestTagIndexUsage(t *testing.T) {
	st, lib := entityFixture(t)
	putTrackCustom(t, st, lib.ID, "/lib/a.flac", "ea", "ca", "A",
		map[string][]string{"MOOD": {"happy"}, "MYKEY": {"foo"}}, true)

	// TagKeys: GROUP BY key over item_tag, counting distinct items. The index leads on
	// key and includes item_id, so this is an index-only (covering) scan.
	tk := queryPlan(t, st, "SELECT key, COUNT(DISTINCT item_id) AS n FROM item_tag GROUP BY key ORDER BY n DESC, key")
	t.Logf("TagKeys plan:\n%s", tk)
	if !strings.Contains(tk, "item_tag_key") {
		t.Errorf("TagKeys should use item_tag_key:\n%s", tk)
	}
	if !strings.Contains(tk, "COVERING INDEX") {
		t.Errorf("TagKeys should be a covering (index-only) scan:\n%s", tk)
	}

	// The facet's item_tag access (itf.key = ?) should use item_tag_key, not a full scan.
	facet := queryPlan(t, st,
		"SELECT itf.value, COUNT(DISTINCT pi.id)"+itemJoins+
			" INNER JOIN item_tag itf ON itf.item_id = pi.id AND itf.key = ? AND itf.value <> '' GROUP BY itf.value",
		"MOOD")
	t.Logf("Facet plan:\n%s", facet)
	if !strings.Contains(facet, "item_tag_key") {
		t.Errorf("facet should use item_tag_key for the item_tag access:\n%s", facet)
	}

	// The per-item EXISTS predicate resolves to an index SEARCH (an exact key/value/item_id
	// seek for equality), never a full-table SCAN of item_tag.
	exists := queryPlan(t, st,
		"SELECT pi.id"+itemJoins+
			" WHERE EXISTS (SELECT 1 FROM item_tag itq WHERE itq.item_id = pi.id AND itq.key = ? AND itq.value = ?)",
		"MOOD", "happy")
	t.Logf("EXISTS plan:\n%s", exists)
	if !strings.Contains(exists, "SEARCH itq") {
		t.Errorf("the per-item EXISTS predicate should be an index seek over item_tag:\n%s", exists)
	}
	if strings.Contains(exists, "SCAN itq") {
		t.Errorf("the per-item EXISTS predicate must not full-scan item_tag:\n%s", exists)
	}
}
