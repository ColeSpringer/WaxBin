package sqlite

import (
	"context"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// TestEnrichWriteScopeClauseBoundsWhatItBinds: a run's reach reaches both write-back
// selects as one statement's bound values, so repeats collapse (albums and release groups
// are each appended by two phases) and the total is capped under SQLite's own ceiling.
// Past the cap the clause widens to everything owed instead: the three lists are ORed in
// one statement, so chunking one of them would re-return every row the other two already
// match, and a reach that wide came from a limit high enough to be a full pass anyway.
func TestEnrichWriteScopeClauseBoundsWhatItBinds(t *testing.T) {
	const itemCol, albumCol = "pi.id", "t.album_id"

	if clause, args := enrichWriteScopeClause(nil, itemCol, albumCol); clause != "" || args != nil {
		t.Errorf("a full run clause = %q with %d args, want no clause at all", clause, len(args))
	}
	if clause, args := enrichWriteScopeClause(&model.EnrichScope{}, itemCol, albumCol); clause != " AND 1=0" || args != nil {
		t.Errorf("an empty scope clause = %q with %d args, want it to reach nothing", clause, len(args))
	}

	clause, args := enrichWriteScopeClause(&model.EnrichScope{
		FieldsItemIDs: []int64{7, 7, 9}, LyricsItemIDs: []int64{7}, BookItemIDs: []int64{9},
		AlbumIDs:        []int64{3, 3},
		ReleaseGroupIDs: []int64{5, 5, 5},
	}, itemCol, albumCol)
	if len(args) != 4 {
		t.Errorf("clause %q binds %v, want each of items 7 and 9, album 3 and group 5 once", clause, args)
	}
	if n := strings.Count(clause, "?"); n != 4 {
		t.Errorf("clause %q holds %d placeholders, want 4 to match the args", clause, n)
	}

	// One over the cap widens to a full run; the cap itself still binds.
	over := make([]int64, maxScopeBinds+1)
	for i := range over {
		over[i] = int64(i + 1)
	}
	if clause, args := enrichWriteScopeClause(&model.EnrichScope{FieldsItemIDs: over}, itemCol, albumCol); clause != "" || args != nil {
		t.Errorf("a reach past the cap gave %d args, want it to widen to everything owed", len(args))
	}
	if _, args := enrichWriteScopeClause(&model.EnrichScope{FieldsItemIDs: over[:maxScopeBinds]}, itemCol, albumCol); len(args) != maxScopeBinds {
		t.Errorf("a reach at the cap bound %d values, want %d", len(args), maxScopeBinds)
	}
}

// TestEnrichmentWritebackRunsAtTheScopeCap: the cap is only worth having if SQLite takes
// a statement that large, so drive the real select with a reach filled to it.
func TestEnrichmentWritebackRunsAtTheScopeCap(t *testing.T) {
	st, _ := entityFixture(t)
	ids := make([]int64, maxScopeBinds)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	if _, err := st.EnrichmentWriteback(context.Background(), &model.EnrichScope{FieldsItemIDs: ids}); err != nil {
		t.Fatalf("writeback at the scope cap: %v", err)
	}
}
