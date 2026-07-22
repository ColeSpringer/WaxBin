package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
)

// limitFixture catalogs six tracks titled A..F with the given per-track durations
// (falling back to 60s), so the canonical sort order is the title order.
func limitFixture(t *testing.T, st *Store, libID int64, durations ...int64) {
	t.Helper()
	titles := []string{"A", "B", "C", "D", "E", "F"}
	for i, title := range titles {
		dur := int64(60_000)
		if i < len(durations) {
			dur = durations[i]
		}
		putTrack(t, st, libID, trackSpec{
			path: fmt.Sprintf("/lib/%s.flac", title), essence: "e" + title, content: "c" + title,
			title: title, artist: "X", album: "Al", durationMS: dur,
		})
	}
}

// setFileSize pins a cataloged file's size for the megabytes-budget tests (the
// scan fixture derives size from its content string, which is unusably small).
func setFileSize(t *testing.T, st *Store, path string, size int64) {
	t.Helper()
	err := st.writeTx(context.Background(), func(tx *sql.Tx) error {
		r, err := tx.ExecContext(context.Background(), "UPDATE file SET size = ? WHERE path = ?", size, []byte(path))
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n != 1 {
			return fmt.Errorf("no file at %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("set file size: %v", err)
	}
}

func TestQueryLimitRandomSeeded(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	limitFixture(t, st, lib.ID)

	draw := func(limit, offset int) []string {
		q := query.New(query.EntityItems).Limit(limit).Offset(offset).LimitBy(query.LimitRandom).Seed(42).Build()
		items, err := st.QueryItems(ctx, q, "")
		if err != nil {
			t.Fatalf("random query: %v", err)
		}
		return titlesOf(items)
	}

	got := draw(3, 0)
	if len(got) != 3 {
		t.Fatalf("random draw = %v, want 3 rows", got)
	}
	// The same seed draws the same rows in the same order; determinism is what
	// the seed is for, so a client can page a stable shuffle.
	if again := draw(3, 0); !equalStrings(got, again) {
		t.Errorf("seeded draw not deterministic: %v vs %v", got, again)
	}
	// The draw is a prefix of the full seeded shuffle, and offset resumes inside
	// that same order.
	full := draw(6, 0)
	if len(full) != 6 || !equalStrings(got, full[:3]) {
		t.Errorf("limited draw %v is not a prefix of the full shuffle %v", got, full)
	}
	if tail := draw(3, 2); !equalStrings(tail, full[2:5]) {
		t.Errorf("offset draw = %v, want shuffle window %v", tail, full[2:5])
	}
	// A different seed yields a different permutation of the same membership
	// (comparing full orders: with 6! possible arrangements an accidental match
	// is vanishingly unlikely).
	q2 := query.New(query.EntityItems).Limit(6).LimitBy(query.LimitRandom).Seed(43).Build()
	items2, err := st.QueryItems(ctx, q2, "")
	if err != nil {
		t.Fatalf("random query seed 43: %v", err)
	}
	if equalStrings(full, titlesOf(items2)) {
		t.Errorf("seeds 42 and 43 produced the identical order %v", full)
	}
}

func TestQueryLimitMinutesBudget(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// A=90s, B=60s, C=30s, D..F=60s; canonical order is A,B,C,D,E,F.
	limitFixture(t, st, lib.ID, 90_000, 60_000, 30_000)

	run := func(q query.Query) []string {
		items, err := st.QueryItems(ctx, q, "")
		if err != nil {
			t.Fatalf("budget query: %v", err)
		}
		return titlesOf(items)
	}

	// 2 minutes: A (90s) fits, B (60s) would overflow, and the fill stops there.
	// C (30s) would still fit the remainder but must not appear: order is
	// authoritative, there is no best-fit skipping.
	got := run(query.New(query.EntityItems).Limit(2).LimitBy(query.LimitMinutes).Build())
	if !equalStrings(got, []string{"A"}) {
		t.Errorf("2-minute budget = %v, want [A] (stop at first overflow, no best-fit)", got)
	}

	// Exact fit is included: A+B+C = 90+60+30s = 3 minutes to the millisecond,
	// so C lands exactly on the remaining budget (admitted; a >= comparison
	// would wrongly drop it) and D (60s > 0 left) ends the fill.
	got = run(query.New(query.EntityItems).Limit(3).LimitBy(query.LimitMinutes).Build())
	if !equalStrings(got, []string{"A", "B", "C"}) {
		t.Errorf("3-minute budget = %v, want [A B C] (== budget is not an overflow)", got)
	}

	// An absurd limit saturates instead of wrapping the budget negative (which
	// would silently return nothing): every priceable row fits.
	got = run(query.New(query.EntityItems).Limit(math.MaxInt).LimitBy(query.LimitMinutes).Build())
	if len(got) != 6 {
		t.Errorf("saturated budget = %v, want all 6 rows", got)
	}

	// A single item larger than the whole budget yields an empty result.
	got = run(query.New(query.EntityItems).Limit(1).LimitBy(query.LimitMinutes).Build())
	if len(got) != 0 {
		t.Errorf("1-minute budget = %v, want empty (first row overflows)", got)
	}

	// Offset skips rows before accumulation begins: from B, a 2-minute budget
	// fits B (60s) + C (30s) and stops at D (60s > 30s left).
	got = run(query.New(query.EntityItems).Limit(2).Offset(1).LimitBy(query.LimitMinutes).Build())
	if !equalStrings(got, []string{"B", "C"}) {
		t.Errorf("offset budget = %v, want [B C]", got)
	}

	// Sorts are honored: by duration descending the fill order is A(90s),B(60s)...
	// and a 2-minute budget again stops after A.
	got = run(query.New(query.EntityItems).Limit(2).LimitBy(query.LimitMinutes).
		OrderBy("duration_ms", true).Build())
	if !equalStrings(got, []string{"A"}) {
		t.Errorf("sorted budget = %v, want [A]", got)
	}

	// Empty sorts + a seed fills in shuffle order: the result must be a prefix of
	// the seeded random order over the same rows.
	seeded := run(query.New(query.EntityItems).Limit(3).LimitBy(query.LimitMinutes).Seed(42).Build())
	shuffle, err := st.QueryItems(ctx, query.New(query.EntityItems).Limit(6).LimitBy(query.LimitRandom).Seed(42).Build(), "")
	if err != nil {
		t.Fatalf("shuffle reference: %v", err)
	}
	ref := titlesOf(shuffle)
	if len(seeded) == 0 || !equalStrings(seeded, ref[:len(seeded)]) {
		t.Errorf("seeded budget fill %v is not a prefix of the seeded shuffle %v", seeded, ref)
	}
}

func TestQueryLimitMegabytes(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	limitFixture(t, st, lib.ID)
	sizes := map[string]int64{"A": 400_000, "B": 400_000, "C": 300_000, "D": 900_000, "E": 100, "F": 100}
	for title, size := range sizes {
		setFileSize(t, st, "/lib/"+title+".flac", size)
	}

	// 1 MB (10^6 bytes): A (400k) + B (400k) fit, C (300k) overflows the 200k
	// remainder and stops the fill.
	q := query.New(query.EntityItems).Limit(1).LimitBy(query.LimitMegabytes).Build()
	items, err := st.QueryItems(ctx, q, "")
	if err != nil {
		t.Fatalf("megabytes query: %v", err)
	}
	if got := titlesOf(items); !equalStrings(got, []string{"A", "B"}) {
		t.Errorf("1 MB budget = %v, want [A B]", got)
	}

	// CountItems ignores the mode entirely: the count is over the full match set.
	if n, err := st.CountItems(ctx, q, ""); err != nil || n != 6 {
		t.Errorf("CountItems under a limit mode = %d (err %v), want 6", n, err)
	}
}

func TestQueryBudgetSkipsUnpriceableRows(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// A has NO measurable duration (an unparsed file); B..F are 60s each.
	limitFixture(t, st, lib.ID, 0)
	// One remote episode with a feed-declared duration but no file: priceable by
	// minutes, unpriceable (0 bytes on hand) by megabytes.
	putFeed(t, st, "http://cast.example/f", "Ep1")

	// A 1-minute budget must skip A rather than admit it free, then fill from B;
	// C overflows the exhausted budget and ends the fill. The episode's 1s feed
	// duration would fit, but the fill already ended at C, so order still rules.
	items, err := st.QueryItems(ctx, query.New(query.EntityItems).
		Limit(1).LimitBy(query.LimitMinutes).Build(), "")
	if err != nil {
		t.Fatalf("minutes query: %v", err)
	}
	if got := titlesOf(items); !equalStrings(got, []string{"B"}) {
		t.Errorf("1-minute budget over an unpriceable row = %v, want [B] (A skipped, not free)", got)
	}

	// A megabytes budget skips the fileless episode (nothing measurable to sync)
	// rather than admitting it free alongside the sized tracks.
	for _, title := range []string{"A", "B", "C", "D", "E", "F"} {
		setFileSize(t, st, "/lib/"+title+".flac", 100_000)
	}
	items, err = st.QueryItems(ctx, query.New(query.EntityItems).
		Limit(1).LimitBy(query.LimitMegabytes).Build(), "")
	if err != nil {
		t.Fatalf("megabytes query: %v", err)
	}
	if got := titlesOf(items); len(got) != 6 {
		t.Errorf("megabytes fill = %v, want the 6 sized tracks and no fileless episode", got)
	}
	for _, title := range titlesOf(items) {
		if title == "Ep1" {
			t.Errorf("fileless episode admitted free into a megabytes budget")
		}
	}
}

func TestQueryLimitMegabytesMultiFileBook(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// A three-part book, 400 KB per part: syncing it costs 1.2 MB, so a 1 MB
	// budget must refuse it. Pricing only the primary part (400 KB) would
	// overflow the device threefold.
	for i, path := range []string{"/lib/bk/1.m4b", "/lib/bk/2.m4b", "/lib/bk/3.m4b"} {
		putBookPart(t, st, lib.ID, path, "bk1", fmt.Sprintf("bp%d", i+1), i)
		setFileSize(t, st, path, 400_000)
	}

	items, err := st.QueryItems(ctx, query.New(query.EntityItems).
		Limit(1).LimitBy(query.LimitMegabytes).Build(), "")
	if err != nil {
		t.Fatalf("1 MB query: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("1 MB budget = %v, want empty (the book costs all three parts)", titlesOf(items))
	}
	items, err = st.QueryItems(ctx, query.New(query.EntityItems).
		Limit(2).LimitBy(query.LimitMegabytes).Build(), "")
	if err != nil {
		t.Fatalf("2 MB query: %v", err)
	}
	if len(items) != 1 || items[0].Kind != model.KindBook {
		t.Errorf("2 MB budget = %v, want just the book", titlesOf(items))
	}
}

func TestQueryLimitMegabytesSharedCUEFile(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	// One 600 KB rip file carved into three virtual tracks. Each included track
	// prices the whole rip file (per-track byte attribution does not exist), so a
	// 1 MB budget fits only the first track: over-counting under-fills the
	// budget, which is the documented safe direction.
	const ripPath = "/lib/rip.flac"
	tracks := make([]model.VirtualTrack, 3)
	for i := range tracks {
		no := i + 1
		title := fmt.Sprintf("Rip %d", no)
		start := int64(i) * 750
		var end int64
		if i < len(tracks)-1 {
			end = start + 750
		}
		tracks[i] = model.VirtualTrack{
			Item: model.PlayableItem{
				Kind: model.KindTrack, State: model.StatePresent,
				Title: title, SortKey: model.SortKey(title),
				IdentityKey: identity.VirtualTrackKey("rip-e", no, start),
			},
			Track:       model.Track{Artist: "Rip Artist", Album: "Rip Album", TrackNo: no},
			StartFrames: start, EndFrames: end,
		}
	}
	if _, err := st.PutScannedVirtualTracks(ctx, model.PutScannedVirtualTracksInput{
		LibraryID: lib.ID,
		File: model.File{
			Path: []byte(ripPath), DisplayPath: ripPath, RelPath: []byte(filepath.Base(ripPath)),
			Kind: model.FileAudio, Size: 600_000, MTimeNS: 1,
			ContentHash: "rip-c", EssenceHash: "rip-e", DurationMS: 30_000, ScanState: model.ScanIndexed,
		},
		Tracks: tracks,
	}); err != nil {
		t.Fatalf("put virtual rip: %v", err)
	}

	q := query.New(query.EntityItems).Limit(1).LimitBy(query.LimitMegabytes).Build()
	items, err := st.QueryItems(ctx, q, "")
	if err != nil {
		t.Fatalf("megabytes query: %v", err)
	}
	if got := titlesOf(items); !equalStrings(got, []string{"Rip 1"}) {
		t.Errorf("shared-rip 1 MB budget = %v, want [Rip 1] (whole file size per virtual track)", got)
	}
}

func TestSmartPlaylistLimitModesEndToEnd(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	limitFixture(t, st, lib.ID)

	// A "3 random" smart playlist evaluates deterministically under its stored
	// seed, through the normal PlaylistItems path, which runs on QueryItems.
	random := query.New(query.EntityItems).Limit(3).LimitBy(query.LimitRandom).Seed(7).Build()
	rpl, err := st.CreatePlaylist(ctx, "Random 3", "", model.PlaylistSmart, "", &random)
	if err != nil {
		t.Fatalf("create random smart playlist: %v", err)
	}
	first, err := st.PlaylistItems(ctx, rpl, "")
	if err != nil {
		t.Fatalf("playlist items: %v", err)
	}
	if len(first) != 3 {
		t.Fatalf("random smart playlist = %v, want 3 members", titlesOf(first))
	}
	again, _ := st.PlaylistItems(ctx, rpl, "")
	if !equalStrings(titlesOf(first), titlesOf(again)) {
		t.Errorf("seeded random playlist unstable: %v vs %v", titlesOf(first), titlesOf(again))
	}

	// A "2 minutes" smart playlist fills the budget in canonical order (every
	// fixture track is 60s, so exactly two fit).
	minutes := query.New(query.EntityItems).Limit(2).LimitBy(query.LimitMinutes).Build()
	mpl, err := st.CreatePlaylist(ctx, "Two Minutes", "", model.PlaylistSmart, "", &minutes)
	if err != nil {
		t.Fatalf("create minutes smart playlist: %v", err)
	}
	items, err := st.PlaylistItems(ctx, mpl, "")
	if err != nil {
		t.Fatalf("minutes playlist items: %v", err)
	}
	if got := titlesOf(items); !equalStrings(got, []string{"A", "B"}) {
		t.Errorf("2-minute smart playlist = %v, want [A B]", got)
	}
}
