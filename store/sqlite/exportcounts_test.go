package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
)

// TestExportCounts pins the manifest's count queries to what an export carries: the
// podcast library, its episodes, and any play state or session on an episode stay
// out, the way Export filters them, so a manifest answered without the body cannot
// disagree with one read from it.
func TestExportCounts(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	track := seedItem(t, st, lib)
	if _, err := st.EnsurePodcastLibrary(ctx, "/podcasts"); err != nil {
		t.Fatal(err)
	}
	putFeed(t, st, "http://cast.example/f", "Ep1")
	var episode string
	if err := st.read.QueryRowContext(ctx, "SELECT pid FROM playable_item WHERE kind = 'episode'").Scan(&episode); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []model.PID{track, model.PID(episode)} {
		if _, err := st.SetStar(ctx, "", pid, true, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := st.RecordSession(ctx, "", pid, "test", 1_600_000_000_000_000_000, 0, 1000); err != nil {
			t.Fatal(err)
		}
	}
	libs, err := st.Libraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := read.ExportCounts{Items: 1, PlayStates: 1, PlaySessions: 1}
	for _, l := range libs {
		if l.Mode != model.ModePodcast {
			want.Libraries++
		}
	}
	if want.Libraries == len(libs) {
		t.Fatal("fixture has no podcast library to filter")
	}

	got, err := st.ExportCounts(ctx)
	if err != nil {
		t.Fatalf("ExportCounts: %v", err)
	}
	if got != want {
		t.Errorf("export counts = %+v, want %+v", got, want)
	}
}
