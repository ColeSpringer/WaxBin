package sqlite

import (
	"strings"
	"testing"
)

// TestEssenceGoneLookupSeeksTheEssenceIndex: a moved file's relink lookup seeks its audio
// within its library on file_essence rather than walking every file of the library.
func TestEssenceGoneLookupSeeksTheEssenceIndex(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	plan := explainPlan(t, st, essenceGoneQ, "e1", lib.ID, "c1")
	if !strings.Contains(plan, "file_essence (essence_hash=? AND library_id=?)") {
		t.Errorf("plan:\n%s\nwant a seek on the essence and the library", plan)
	}
}

// TestScopedFileIndexSeeksThePathRange: a sub-path scan's two index reads seek the paths
// under the sub-path instead of walking the whole library's files.
func TestScopedFileIndexSeeksThePathRange(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	lo := []byte("/lib/A/")
	files, fargs, aux, aargs := scopedFileIndexQueries(lib.ID, lo, prefixUpperBound(lo))
	for _, q := range []struct {
		stmt string
		args []any
	}{{files, fargs}, {aux, aargs}} {
		plan := explainPlan(t, st, q.stmt, q.args...)
		if strings.Contains(plan, "file_library") || !strings.Contains(plan, "path>? AND path<?") {
			t.Errorf("%s\nplan:\n%s\nwant a seek on the path range", q.stmt, plan)
		}
	}
}

// TestPlaylistNameSeedSeeksTheNameIndex: a lookup by file name alone, folded for case,
// seeks file_name_nocase.
func TestPlaylistNameSeedSeeksTheNameIndex(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	plan := explainPlan(t, st, "SELECT id FROM file WHERE name = ? COLLATE NOCASE", "01 Song.flac")
	if !strings.Contains(plan, "file_name_nocase (name=?)") {
		t.Errorf("plan:\n%s\nwant a seek on file_name_nocase", plan)
	}
}

// TestDisplayPathFoldSeeksItsIndex: the case-folded collision check an import makes seeks
// file_display_nocase.
func TestDisplayPathFoldSeeksItsIndex(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	plan := explainPlan(t, st, displayPathFoldQ, "/lib/A/01.flac")
	if !strings.Contains(plan, "file_display_nocase (display_path=?)") {
		t.Errorf("plan:\n%s\nwant a seek on file_display_nocase", plan)
	}
}
