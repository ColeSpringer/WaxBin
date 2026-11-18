package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/colespringer/waxbin/art"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
)

// benchCover is a small distinct picture per n.
func benchCover(tb testing.TB, n int) *model.ArtImage {
	tb.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	img.Set(0, 0, color.RGBA{uint8(n), uint8(n >> 8), 7, 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		tb.Fatal(err)
	}
	data := buf.Bytes()
	return &model.ArtImage{Data: data, Format: "png", Width: 8, Height: 8, Hash: art.Hash(data),
		Attribution: model.Attribution{Source: model.SourceTag}}
}

// artBenchShapes are the catalogs the art walk is measured over, by where a track's
// front cover is found: on the track, on a sibling in its album, on its release group,
// and nowhere.
var artBenchShapes = []string{"own", "sibling", "group", "none"}

// artBenchStore seeds benchScale tracks, 25 to an album, with covers placed for shape,
// and returns the tracks the shape is about. In the sibling shape those are the half of
// each album that carries no cover.
func artBenchStore(tb testing.TB, shape string) (*Store, []model.PID) {
	tb.Helper()
	st, lib := benchStore(tb)
	ctx := context.Background()
	var pids []model.PID
	for i := range benchScale {
		in := benchTrack(lib.ID, i)
		album := i % 200
		covered := shape == "own" || shape == "sibling" && (i/200)%2 == 0
		if covered {
			in.CoverArt = benchCover(tb, album)
		}
		res, err := st.PutScannedTrack(ctx, in)
		if err != nil {
			tb.Fatal(err)
		}
		if shape != "sibling" || !covered {
			pids = append(pids, res.ItemPID)
		}
	}
	if shape == "group" {
		rows, err := st.rdb().QueryContext(ctx, "SELECT id FROM release_group")
		if err != nil {
			tb.Fatal(err)
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				tb.Fatal(err)
			}
			ids = append(ids, id)
		}
		if err := rows.Close(); err != nil {
			tb.Fatal(err)
		}
		if err := rows.Err(); err != nil {
			tb.Fatal(err)
		}
		for n, id := range ids {
			pic, err := st.examineArt(ctx, benchCover(tb, n))
			if err != nil {
				tb.Fatal(err)
			}
			if err := st.writeTx(ctx, func(tx *sql.Tx) error {
				_, err := setEntityArtRoleTx(ctx, tx, "release_group", id, "front", pic)
				return err
			}); err != nil {
				tb.Fatal(err)
			}
		}
	}
	return st, pids
}

// BenchmarkArtWalk measures the front-cover walk over a 5k-track catalog: one walk, the
// read every art request starts with, and the hashes of a 50-item page in one call,
// beside the page read itself for scale. The page is fifty tracks of fifty albums, the
// worst case for the rungs a page's items could share.
//
// Measured 2026-11-18 on a busy machine held to 12 threads, so read the ratios:
//
//	shape     one     50 hashes   page
//	own       39µs    0.34ms      0.8ms
//	sibling   58µs    0.47ms      0.7ms
//	group     56µs    0.55ms      0.7ms
//	none      59µs    0.6ms       0.7ms
//
// The chain walk this replaced ran one entity at a time, 105 to 187µs each, so a page's
// fifty cost 3.3 to 7ms.
//
// cards and facet are the reads that carry ArtHash themselves, fifty album cards and a
// 200-bucket album facet. Timed in one run beside the walk they make, the walk is about
// a tenth of each: 0.4ms of a 4ms page of cards, 1.6ms of a 16ms facet. A search's hits
// carry theirs for 0.3 to 0.7ms of a 12 to 18ms search over this catalog.
func BenchmarkArtWalk(b *testing.B) {
	for _, shape := range artBenchShapes {
		st, pids := artBenchStore(b, shape)
		ctx := context.Background()
		// Fifty tracks of fifty albums, so no two share a rung.
		refs := make([]model.EntityRef, 50)
		for i := range refs {
			refs[i] = model.EntityRef{Type: model.ArtTrack, PID: pids[i*97%len(pids)]}
		}
		b.Run(shape+"/one", func(b *testing.B) {
			i := 0
			for b.Loop() {
				_, _ = st.ArtProvenance(ctx, model.EntityRef{Type: model.ArtTrack, PID: pids[i%len(pids)]}, model.ArtRoleFront)
				i++
			}
		})
		b.Run(shape+"/hashes", func(b *testing.B) {
			for b.Loop() {
				if _, err := st.ArtHashes(ctx, refs); err != nil {
					b.Fatal(err)
				}
			}
		})
		q := query.New(query.EntityItems).Build()
		b.Run(shape+"/page", func(b *testing.B) {
			for b.Loop() {
				if _, err := st.QueryPage(ctx, q, "", 50, false, ""); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(shape+"/cards", func(b *testing.B) {
			for b.Loop() {
				if _, err := st.EntityPage(ctx, read.EntityAlbum, "", 50); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(shape+"/facet", func(b *testing.B) {
			for b.Loop() {
				if _, err := st.Facet(ctx, q, read.GroupAlbum, "", 0, ""); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
