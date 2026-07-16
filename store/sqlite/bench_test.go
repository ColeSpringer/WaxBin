package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
)

// benchStore opens a fresh catalog for a benchmark.
func benchStore(tb testing.TB) (*Store, *model.Library) {
	tb.Helper()
	ctx := context.Background()
	st, err := Open(ctx, OpenOptions{Path: filepath.Join(tb.TempDir(), "c.db"), Owner: "bench"})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = st.Close() })
	lib, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte("/lib"), DisplayRoot: "/lib", Mode: model.ModeManaged, Profile: "waxbin-native",
	})
	if err != nil {
		tb.Fatal(err)
	}
	return st, lib
}

// benchInsert persists one synthetic track with realistic entity spread (50
// artists, 200 albums, 5 genres) so the read benchmarks exercise real grouping.
func benchInsert(tb testing.TB, st *Store, libID int64, i int) {
	tb.Helper()
	artist := fmt.Sprintf("Artist %d", i%50)
	album := fmt.Sprintf("Album %d", i%200)
	genre := []string{"Rock", "Jazz", "Pop", "Electronic", "Classical"}[i%5]
	path := fmt.Sprintf("/lib/%d/%d.flac", i%200, i)
	title := fmt.Sprintf("Track %d", i)
	in := model.PutScannedTrackInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte(path), DisplayPath: path, RelPath: fmt.Appendf(nil, "%d.flac", i),
			Kind: model.FileAudio, ContentHash: fmt.Sprintf("c%d", i), EssenceHash: fmt.Sprintf("e%d", i),
			DurationMS: 200000, ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: title,
			SortKey: model.SortKey(title), IdentityKey: "essence:e" + fmt.Sprint(i),
		},
		Track: model.Track{
			Artist: artist, ArtistSort: model.SortKey(artist), Album: album, AlbumArtist: artist,
			Genre: genre, Genres: []string{genre}, Year: 2000 + i%20,
		},
	}
	if _, err := st.PutScannedTrack(context.Background(), in); err != nil {
		tb.Fatal(err)
	}
}

func populate(tb testing.TB, st *Store, libID int64, n int) {
	tb.Helper()
	for i := range n {
		benchInsert(tb, st, libID, i)
	}
}

const benchScale = 5000

// BenchmarkPutScannedTrack measures the scan-persist hot path (one atomic write
// txn per track, including entity resolution, FTS, and maintained rollups).
func BenchmarkPutScannedTrack(b *testing.B) {
	st, lib := benchStore(b)
	// Each iteration must insert a distinct track (distinct essence and identity),
	// so a manual counter supplies the per-iteration index that b.Loop does not.
	i := 0
	for b.Loop() {
		benchInsert(b, st, lib.ID, i)
		i++
	}
}

// BenchmarkQueryPageAtScale measures keyset pagination over a 5k-track catalog.
// The browse hot path must stay cheap regardless of catalog size.
func BenchmarkQueryPageAtScale(b *testing.B) {
	st, lib := benchStore(b)
	populate(b, st, lib.ID, benchScale)
	q := query.New(query.EntityItems).Build()
	for b.Loop() {
		if _, err := st.QueryPage(context.Background(), q, "", 50, false); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkFacetGenreAtScale measures the genre facet aggregation at scale.
func BenchmarkFacetGenreAtScale(b *testing.B) {
	st, lib := benchStore(b)
	populate(b, st, lib.ID, benchScale)
	q := query.New(query.EntityItems).Build()
	for b.Loop() {
		if _, err := st.Facet(context.Background(), q, read.GroupGenre); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkBrowseNewestAtScale measures a discovery-list page at scale.
func BenchmarkBrowseNewestAtScale(b *testing.B) {
	st, lib := benchStore(b)
	populate(b, st, lib.ID, benchScale)
	for b.Loop() {
		if _, err := st.BrowsePage(context.Background(), read.ListNewest, read.BrowseOptions{Limit: 50}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSearchAtScale measures grouped BM25 search at scale.
func BenchmarkSearchAtScale(b *testing.B) {
	st, lib := benchStore(b)
	populate(b, st, lib.ID, benchScale)
	for b.Loop() {
		if _, err := st.Search(context.Background(), "track", read.SearchOptions{Limit: 20}); err != nil {
			b.Fatal(err)
		}
	}
}
