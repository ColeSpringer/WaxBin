package sqlite

import (
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// TestEnrichWriteScopeClauseBindsEachListOnce: a run's reach reaches both write-back
// selects as one statement, each list bound once as a JSON array whatever its length,
// with repeats collapsed (albums and release groups are each appended by two phases).
func TestEnrichWriteScopeClauseBindsEachListOnce(t *testing.T) {
	const itemCol, albumCol = "pi.id", "t.album_id"

	if clause, args := enrichWriteScopeClause(nil, itemCol, albumCol); clause != "" || args != nil {
		t.Errorf("a full run clause = %q with %d args, want no clause at all", clause, len(args))
	}
	if clause, args := enrichWriteScopeClause(&model.EnrichScope{}, itemCol, albumCol); clause != " AND 1=0" || args != nil {
		t.Errorf("an empty scope clause = %q with %d args, want it to reach nothing", clause, len(args))
	}

	clause, args := enrichWriteScopeClause(&model.EnrichScope{
		FieldsItemIDs: []int64{9, 7, 7}, LyricsItemIDs: []int64{7}, BookItemIDs: []int64{9},
		AlbumIDs:        []int64{3, 3},
		ReleaseGroupIDs: []int64{5, 5, 5},
	}, itemCol, albumCol)
	if want := []any{"[7,9]", "[3]", "[5]"}; len(args) != len(want) || args[0] != want[0] || args[1] != want[1] || args[2] != want[2] {
		t.Errorf("clause %q binds %v, want %v", clause, args, want)
	}
	if n := strings.Count(clause, "?"); n != 3 {
		t.Errorf("clause %q holds %d placeholders, want 3 to match the args", clause, n)
	}

	wide := make([]int64, 100000)
	for i := range wide {
		wide[i] = int64(i + 1)
	}
	if clause, args := enrichWriteScopeClause(&model.EnrichScope{FieldsItemIDs: wide}, itemCol, albumCol); len(args) != 1 || strings.Count(clause, "?") != 1 {
		t.Errorf("a wide reach gave %q with %d args, want one bound array", clause, len(args))
	}
}
