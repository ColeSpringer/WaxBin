package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
)

func bucketByDisplay(r *read.FacetResult, display string) (read.Bucket, bool) {
	for _, b := range r.Buckets {
		if b.Display == display {
			return b, true
		}
	}
	return read.Bucket{}, false
}

func TestFacetByGenre(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1", title: "A", artist: "X", album: "Al", genre: "Rock; Pop"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/2.flac", essence: "e2", content: "c2", title: "B", artist: "Y", album: "Bl", genre: "Rock"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/3.flac", essence: "e3", content: "c3", title: "C", artist: "Z", album: "Cl", genre: ""})

	res, err := st.Facet(ctx, query.New(query.EntityItems).Build(), read.GroupGenre, "")
	if err != nil {
		t.Fatalf("facet: %v", err)
	}
	rock, ok := bucketByDisplay(res, "Rock")
	if !ok || rock.Count != 2 {
		t.Errorf("Rock bucket = %+v, want count 2", rock)
	}
	if rock.EntityPID == "" {
		t.Error("genre bucket should carry the entity pid for drilldown")
	}
	pop, ok := bucketByDisplay(res, "Pop")
	if !ok || pop.Count != 1 {
		t.Errorf("Pop bucket = %+v, want count 1", pop)
	}
	noGenre, ok := bucketByDisplay(res, read.NoGenre)
	if !ok || noGenre.Count != 1 || !noGenre.IsUnknown {
		t.Errorf("No-Genre bucket = %+v, want count 1 + unknown", noGenre)
	}
}

func TestFacetByArtistUnknownBucket(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1", title: "A", artist: "Radiohead", album: "OK"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/2.flac", essence: "e2", content: "c2", title: "B", artist: "", album: ""})

	res, err := st.Facet(ctx, query.New(query.EntityItems).Build(), read.GroupArtist, "")
	if err != nil {
		t.Fatalf("facet: %v", err)
	}
	if b, ok := bucketByDisplay(res, "Radiohead"); !ok || b.Count != 1 {
		t.Errorf("Radiohead bucket = %+v, want count 1", b)
	}
	if b, ok := bucketByDisplay(res, read.UnknownArtist); !ok || b.Count != 1 || !b.IsUnknown {
		t.Errorf("Unknown-Artist bucket = %+v, want count 1 + unknown", b)
	}
}

func TestFacetByYearAndKind(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1", title: "A", artist: "X", album: "Al", year: 1997})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/2.flac", essence: "e2", content: "c2", title: "B", artist: "Y", album: "Bl", year: 1997})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/3.flac", essence: "e3", content: "c3", title: "C", artist: "Z", album: "Cl"}) // no year

	res, err := st.Facet(ctx, query.New(query.EntityItems).Build(), read.GroupYear, "")
	if err != nil {
		t.Fatalf("facet year: %v", err)
	}
	if b, ok := bucketByDisplay(res, "1997"); !ok || b.Count != 2 {
		t.Errorf("1997 bucket = %+v, want count 2", b)
	}
	if b, ok := bucketByDisplay(res, read.UnknownYear); !ok || b.Count != 1 {
		t.Errorf("Unknown-Year bucket = %+v, want count 1", b)
	}

	kindRes, err := st.Facet(ctx, query.New(query.EntityItems).Build(), read.GroupKind, "")
	if err != nil {
		t.Fatalf("facet kind: %v", err)
	}
	if b, ok := bucketByDisplay(kindRes, string(model.KindTrack)); !ok || b.Count != 3 {
		t.Errorf("track kind bucket = %+v, want count 3", b)
	}
}

func TestFacetHonorsFilter(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1", title: "A", artist: "X", album: "Al", genre: "Rock", year: 2000})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/2.flac", essence: "e2", content: "c2", title: "B", artist: "Y", album: "Bl", genre: "Rock", year: 1990})

	q := query.New(query.EntityItems).Where("year", query.OpGte, 2000).Build()
	res, err := st.Facet(ctx, q, read.GroupGenre, "")
	if err != nil {
		t.Fatalf("facet: %v", err)
	}
	rock, ok := bucketByDisplay(res, "Rock")
	if !ok || rock.Count != 1 {
		t.Errorf("filtered Rock bucket = %+v, want count 1 (year>=2000)", rock)
	}
}

func TestFacetRejectsBadGroupBy(t *testing.T) {
	st, _ := entityFixture(t)
	if _, err := st.Facet(context.Background(), query.New(query.EntityItems).Build(), read.GroupBy("bogus"), ""); err == nil {
		t.Fatal("expected an error for an unsupported group-by")
	}
}

func TestQueryPageKeysetCoversAllOnce(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	titles := []string{"Echo", "Alpha", "Delta", "Bravo", "Charlie"}
	for i, title := range titles {
		putTrack(t, st, lib.ID, trackSpec{
			path:    "/lib/" + title + ".flac",
			essence: "e" + title, content: "c" + title, title: title, artist: "X", album: "Al",
		})
		_ = i
	}

	seen := map[model.PID]int{}
	var order []string
	cursor := read.Cursor("")
	pages := 0
	for {
		page, err := st.QueryPage(ctx, query.New(query.EntityItems).Build(), cursor, 2, false, "")
		if err != nil {
			t.Fatalf("page: %v", err)
		}
		for _, it := range page.Items {
			seen[it.PID]++
			order = append(order, it.Title)
		}
		pages++
		if !page.HasMore {
			break
		}
		cursor = page.Next
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}

	if len(seen) != len(titles) {
		t.Fatalf("saw %d distinct items, want %d", len(seen), len(titles))
	}
	for pid, n := range seen {
		if n != 1 {
			t.Errorf("item %s returned %d times, want exactly 1", pid, n)
		}
	}
	want := []string{"Alpha", "Bravo", "Charlie", "Delta", "Echo"} // collation order
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("page order[%d] = %q, want %q (full order %v)", i, order[i], want[i], order)
		}
	}
}

func TestQueryPageDescending(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	for _, title := range []string{"Alpha", "Bravo", "Charlie"} {
		putTrack(t, st, lib.ID, trackSpec{
			path: "/lib/" + title + ".flac", essence: "e" + title, content: "c" + title,
			title: title, artist: "X", album: "Al",
		})
	}

	var order []string
	cursor := read.Cursor("")
	for {
		page, err := st.QueryPage(ctx, query.New(query.EntityItems).Build(), cursor, 2, true, "") // desc
		if err != nil {
			t.Fatalf("page: %v", err)
		}
		for _, it := range page.Items {
			order = append(order, it.Title)
		}
		if !page.HasMore {
			break
		}
		cursor = page.Next
	}
	want := []string{"Charlie", "Bravo", "Alpha"} // reverse collation order, no dups
	if len(order) != len(want) {
		t.Fatalf("desc paging returned %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("desc order[%d] = %q, want %q (full %v)", i, order[i], want[i], order)
		}
	}
}

func TestQueryPageStableUnderConcurrentInsert(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	for _, title := range []string{"Alpha", "Charlie", "Echo"} {
		putTrack(t, st, lib.ID, trackSpec{
			path: "/lib/" + title + ".flac", essence: "e" + title, content: "c" + title,
			title: title, artist: "X", album: "Al",
		})
	}

	page1, err := st.QueryPage(ctx, query.New(query.EntityItems).Build(), "", 2, false, "")
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1.Items) != 2 || !page1.HasMore {
		t.Fatalf("page1 = %d items, hasMore=%v; want 2 + more", len(page1.Items), page1.HasMore)
	}

	// Insert rows that sort before (Bravo) and after (Delta) the cursor position.
	for _, title := range []string{"Bravo", "Delta"} {
		putTrack(t, st, lib.ID, trackSpec{
			path: "/lib/" + title + ".flac", essence: "e" + title, content: "c" + title,
			title: title, artist: "X", album: "Al",
		})
	}

	page2, err := st.QueryPage(ctx, query.New(query.EntityItems).Build(), page1.Next, 10, false, "")
	if err != nil {
		t.Fatalf("page2: %v", err)
	}

	seen := map[string]bool{}
	for _, it := range append(append([]*model.ItemView{}, page1.Items...), page2.Items...) {
		if seen[it.Title] {
			t.Errorf("title %q returned on more than one page (keyset must not duplicate)", it.Title)
		}
		seen[it.Title] = true
	}
	// Every item present when pagination started and sorting after the cursor
	// must still be returned (no skips): Alpha, Charlie from page1; Echo after.
	for _, title := range []string{"Alpha", "Charlie", "Echo"} {
		if !seen[title] {
			t.Errorf("item %q present at page-1 time was skipped across pages", title)
		}
	}
}
