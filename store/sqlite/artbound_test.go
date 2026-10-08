package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/png"
	"testing"

	"github.com/colespringer/waxbin/art"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// widePNG is an opaque picture of the given size with a photograph's texture, a gradient
// under faint noise, so a scaled copy of it is smaller as a JPEG.
func widePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	seed := uint32(88172645)
	for y := range h {
		for x := range w {
			seed ^= seed << 13
			seed ^= seed >> 17
			seed ^= seed << 5
			i := img.PixOffset(x, y)
			img.Pix[i] = uint8(x*255/w) + uint8(seed&7)
			img.Pix[i+1] = uint8(y*255/h) + uint8((seed>>8)&7)
			img.Pix[i+2] = 96 + uint8((seed>>16)&7)
			img.Pix[i+3] = 255
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// claimedPNG is a PNG whose header declares w x h and whose image data is junk, so it
// probes as a picture of that size and will not decode.
func claimedPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	chunk := func(typ string, data []byte) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(data)))
		buf.Write(n[:])
		body := append([]byte(typ), data...)
		buf.Write(body)
		binary.BigEndian.PutUint32(n[:], crc32.ChecksumIEEE(body))
		buf.Write(n[:])
	}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], uint32(w))
	binary.BigEndian.PutUint32(ihdr[4:], uint32(h))
	ihdr[8], ihdr[9] = 8, 2
	chunk("IHDR", ihdr)
	chunk("IDAT", []byte("not a deflate stream"))
	chunk("IEND", nil)
	return buf.Bytes()
}

// taggedCover is the carrier the scan hands over for an embedded picture.
func taggedCover(t *testing.T, raw []byte) *model.ArtImage {
	t.Helper()
	info := art.Describe(raw)
	if info.Format == "" || info.Width == 0 {
		t.Fatalf("describe cover: %+v, want a decoded image", info)
	}
	return &model.ArtImage{
		Data: raw, Hash: info.Hash, Format: info.Format, Width: info.Width, Height: info.Height,
		Attribution: model.Attribution{Source: model.SourceTag},
	}
}

// putCovered writes a scanned track carrying cover.
func putCovered(t *testing.T, st *Store, libID int64, spec trackSpec, cover *model.ArtImage) model.PID {
	t.Helper()
	in := trackSpecInput(libID, spec)
	in.CoverArt = cover
	res, err := st.PutScannedTrack(context.Background(), in)
	if err != nil {
		t.Fatalf("put %s: %v", spec.path, err)
	}
	return res.ItemPID
}

// TestStoredArtIsBounded: a picture over art.MaxSourceDim is stored fitted to it,
// art_resized remembers the arriving bytes against the stored source, and the same
// picture arriving again, raw or as the bytes a write-back embedded, changes nothing.
func TestStoredArtIsBounded(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	raw := widePNG(t, art.MaxSourceDim+200, (art.MaxSourceDim+200)/2)
	spec := trackSpec{path: "/lib/Al/1.flac", essence: "e1", content: "c1",
		title: "One", artist: "X", albumArt: "X", album: "Al"}
	pid := putCovered(t, st, lib.ID, spec, taggedCover(t, raw))
	ref := model.EntityRef{Type: model.ArtTrack, PID: pid}

	blob, err := st.ResolveArt(ctx, ref, model.ArtRoleFront, 0)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	format, w, h, err := art.Probe(blob.Bytes)
	if err != nil || format != "jpeg" || w != art.MaxSourceDim || h != art.MaxSourceDim/2 {
		t.Fatalf("stored source probes as %s %dx%d (err %v), want jpeg %dx%d",
			format, w, h, err, art.MaxSourceDim, art.MaxSourceDim/2)
	}
	if blob.SourceHash != art.Hash(blob.Bytes) {
		t.Errorf("source hash %s is not the hash of its bytes %s", blob.SourceHash, art.Hash(blob.Bytes))
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM art_source"); n != 1 {
		t.Errorf("art_source rows = %d, want the bounded picture alone", n)
	}
	if got := scalarStr(t, st, "SELECT hash FROM art_resized WHERE from_hash = ?", art.Hash(raw)); got != blob.SourceHash {
		t.Errorf("art_resized maps the arriving bytes to %q, want the stored source %s", got, blob.SourceHash)
	}
	prov, err := st.ArtProvenance(ctx, ref, model.ArtRoleFront)
	if err != nil || prov.Width != art.MaxSourceDim || prov.Format != "jpeg" || prov.Size != len(blob.Bytes) {
		t.Errorf("provenance = %+v (err %v), want the stored jpeg, %d wide and %d bytes",
			prov, err, art.MaxSourceDim, len(blob.Bytes))
	}

	updated := scalarInt(t, st, "SELECT updated_at FROM art_map WHERE role = 'front'")
	putCovered(t, st, lib.ID, spec, taggedCover(t, raw))        // a rescan of the same file
	putCovered(t, st, lib.ID, spec, taggedCover(t, blob.Bytes)) // what a write-back embedded, read back
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM art_source"); n != 1 {
		t.Errorf("art_source rows after the rescans = %d, want 1", n)
	}
	if got := scalarInt(t, st, "SELECT updated_at FROM art_map WHERE role = 'front'"); got != updated {
		t.Errorf("a rescan of the same picture rewrote the front mapping")
	}
}

// TestABoundedCoverIsMatchedWithoutDecoding: the memo answers before any decode, so a
// picture it maps is attached as whatever source it names.
func TestABoundedCoverIsMatchedWithoutDecoding(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	first := putCovered(t, st, lib.ID, trackSpec{path: "/lib/Al/1.flac", essence: "e1", content: "c1",
		title: "One", artist: "X", albumArt: "X", album: "Al"}, taggedCover(t, widePNG(t, 40, 20)))
	small, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: first}, model.ArtRoleFront, 0)
	if err != nil {
		t.Fatalf("resolve the small cover: %v", err)
	}
	// Bytes that only claim to be a wide picture: a path that decoded first would store
	// them as they are, so the memo answering is what proves it was read first.
	raw := []byte("not a picture, whatever the carrier says")
	if _, err := st.write.ExecContext(ctx, "INSERT INTO art_resized(from_hash, hash) VALUES (?, ?)",
		art.Hash(raw), small.SourceHash); err != nil {
		t.Fatalf("plant the memo: %v", err)
	}
	claimed := &model.ArtImage{Data: raw, Hash: art.Hash(raw), Format: "png",
		Width: art.MaxSourceDim + 200, Height: (art.MaxSourceDim + 200) / 2,
		Attribution: model.Attribution{Source: model.SourceTag}}
	second := putCovered(t, st, lib.ID, trackSpec{path: "/lib/Bl/1.flac", essence: "e2", content: "c2",
		title: "Two", artist: "Y", albumArt: "Y", album: "Bl"}, claimed)
	got, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: second}, model.ArtRoleFront, 0)
	if err != nil || got.SourceHash != small.SourceHash {
		t.Errorf("the oversized picture resolved to %+v (err %v), want the memo's source %s with no decode",
			got, err, small.SourceHash)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM art_source"); n != 1 {
		t.Errorf("art_source rows = %d, want the one the memo names", n)
	}
}

// TestUserArtIsBounded: a picture a person sets by hand is bounded the same way, and
// setting the same picture again owes the files nothing, since the cover did not move.
func TestUserArtIsBounded(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	pid := putTrack(t, st, lib.ID, trackSpec{path: "/lib/Al/1.flac", essence: "e1", content: "c1",
		title: "One", artist: "X", albumArt: "X", album: "Al"}).ItemPID
	raw := widePNG(t, 600, art.MaxSourceDim+400)
	set := func() {
		t.Helper()
		if _, err := st.SetItemArt(ctx, pid, model.ArtRoleFront, raw, "",
			model.Attribution{Source: model.SourceUser}, model.LockUnchanged, true); err != nil {
			t.Fatalf("SetItemArt: %v", err)
		}
	}
	set()
	ref := model.EntityRef{Type: model.ArtTrack, PID: pid}
	roles, err := st.ArtRoles(ctx, ref)
	if err != nil || len(roles) != 1 || roles[0].Height != art.MaxSourceDim || roles[0].Format != "jpeg" {
		t.Fatalf("art roles = %+v (err %v), want one jpeg front %d tall", roles, err, art.MaxSourceDim)
	}
	const owedQ = "SELECT COUNT(*) FROM file_diagnostic WHERE code = ?"
	if n := scalarInt(t, st, owedQ, string(model.DiagTagWriteOwed)); n == 0 {
		t.Fatal("the first set owed the file no write-back")
	}
	if _, err := st.write.ExecContext(ctx, "DELETE FROM file_diagnostic WHERE code = ?", string(model.DiagTagWriteOwed)); err != nil {
		t.Fatal(err)
	}
	set()
	if n := scalarInt(t, st, owedQ, string(model.DiagTagWriteOwed)); n != 0 {
		t.Errorf("setting the same picture again owed %d write-backs, want none", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM art_source"); n != 1 {
		t.Errorf("art_source rows = %d, want 1", n)
	}
}

// TestGCArtDropsTheResizedMemo: the memo goes with its source.
func TestGCArtDropsTheResizedMemo(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	pid := putCovered(t, st, lib.ID, trackSpec{path: "/lib/Al/1.flac", essence: "e1", content: "c1",
		title: "One", artist: "X", albumArt: "X", album: "Al"},
		taggedCover(t, widePNG(t, art.MaxSourceDim+200, 300)))
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM art_resized"); n != 1 {
		t.Fatalf("art_resized rows = %d, want 1", n)
	}
	if _, err := st.SetItemArt(ctx, pid, model.ArtRoleFront, widePNG(t, 40, 20), "",
		model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false); err != nil {
		t.Fatalf("SetItemArt: %v", err)
	}
	sources, _, err := st.GCArt(ctx)
	if err != nil || sources != 1 {
		t.Fatalf("GCArt collected %d sources (err %v), want the bounded picture", sources, err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM art_resized"); n != 0 {
		t.Errorf("art_resized rows after GC = %d, want 0", n)
	}
}

// TestAnUndecodableOversizedPictureIsExaminedOnce: a picture that probes past the bound
// but will not decode is kept as it arrived, and the memo maps it to itself so the next
// arrival costs no decode.
func TestAnUndecodableOversizedPictureIsExaminedOnce(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	raw := claimedPNG(t, art.MaxSourceDim+200, 400)
	spec := trackSpec{path: "/lib/Al/1.flac", essence: "e1", content: "c1",
		title: "One", artist: "X", albumArt: "X", album: "Al"}
	putCovered(t, st, lib.ID, spec, taggedCover(t, raw))
	hash := art.Hash(raw)
	if got := scalarStr(t, st, "SELECT source_hash FROM art_map WHERE role = 'front'"); got != hash {
		t.Fatalf("the front maps %q, want the arrival %s kept as it was", got, hash)
	}
	if got := scalarStr(t, st, "SELECT hash FROM art_resized WHERE from_hash = ?", hash); got != hash {
		t.Errorf("the memo maps the arrival to %q, want itself", got)
	}
	putCovered(t, st, lib.ID, spec, taggedCover(t, raw))
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM art_source"); n != 1 {
		t.Errorf("art_source rows after the second arrival = %d, want 1", n)
	}
}

// TestTheWriteTrustsAnExaminedPicture: the store examines an oversized picture before
// its write and hands the write a carrier marked with the arrival's hash; the writers
// take nothing else, store the carrier as given, decoding nothing, and record the pair.
func TestTheWriteTrustsAnExaminedPicture(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	raw := widePNG(t, art.MaxSourceDim+200, (art.MaxSourceDim+200)/2)
	examined, err := st.examineArt(ctx, taggedCover(t, raw))
	if err != nil || examined.from != art.Hash(raw) || examined.img.Width != art.MaxSourceDim ||
		examined.img.Hash != art.Hash(examined.img.Data) {
		t.Fatalf("examineArt = %+v (err %v); want a %d-wide copy marked with the arrival's hash", examined, err, art.MaxSourceDim)
	}

	pid := putTrack(t, st, lib.ID, trackSpec{path: "/lib/Al/1.flac", essence: "e1", content: "c1",
		title: "One", artist: "X", albumArt: "X", album: "Al"}).ItemPID
	itemID := int64(scalarInt(t, st, "SELECT id FROM playable_item WHERE pid = ?", string(pid)))
	junk := []byte("examined elsewhere, says the carrier")
	marked := examinedArt{img: &model.ArtImage{Data: junk, Hash: art.Hash(junk), Format: "png", Width: 10, Height: 10,
		Attribution: model.Attribution{Source: model.SourceUser}}, from: "the-arrival"}
	if err := st.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := setEntityArtRoleTx(ctx, tx, "track", itemID, "front", marked)
		return err
	}); err != nil {
		t.Fatalf("write the marked carrier: %v", err)
	}
	if got := scalarStr(t, st, "SELECT hash FROM art_source"); got != art.Hash(junk) {
		t.Errorf("stored source = %q, want the carrier's bytes as given", got)
	}
	if got := scalarStr(t, st, "SELECT hash FROM art_resized WHERE from_hash = 'the-arrival'"); got != art.Hash(junk) {
		t.Errorf("the memo maps the arrival to %q, want the stored source", got)
	}
}

// TestExamineArtMeasuresThePictureItself: a producer's dimensions do not decide the
// bound; the bytes do, so a picture reported small is still scaled when it is not.
func TestExamineArtMeasuresThePictureItself(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	raw := widePNG(t, art.MaxSourceDim+200, (art.MaxSourceDim+200)/2)
	lying := &model.ArtImage{Data: raw, Hash: art.Hash(raw), Format: "png", Width: 100, Height: 50,
		Attribution: model.Attribution{Source: model.SourceTag}}
	examined, err := st.examineArt(context.Background(), lying)
	if err != nil || examined.img.Width != art.MaxSourceDim || examined.from != art.Hash(raw) {
		t.Errorf("examineArt(a picture reported 100x50) = %dx%d from %q (err %v), want it scaled to %d wide",
			examined.img.Width, examined.img.Height, examined.from, err, art.MaxSourceDim)
	}
}

// TestExaminingOnAClosedStoreAnswersAsAWriteWould: an oversized picture set on a
// suspended store is refused the way any write is, rather than failing on the closed
// read pool first.
func TestExaminingOnAClosedStoreAnswersAsAWriteWould(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	pid := putTrack(t, st, lib.ID, trackSpec{path: "/lib/Al/1.flac", essence: "e1", content: "c1",
		title: "One", artist: "X", albumArt: "X", album: "Al"}).ItemPID
	if err := st.Suspend(); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	_, err := st.SetItemArt(ctx, pid, model.ArtRoleFront, widePNG(t, art.MaxSourceDim+200, 300), "",
		model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false)
	if !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Errorf("set on a suspended store = %v, want the write refusal (CodeUnsupported)", err)
	}
	if _, err := st.Reopen(ctx); err != nil {
		t.Fatalf("reopen: %v", err)
	}
}

// TestALockedItemsOversizedCoverIsExaminedOnce: a cover the lock keeps from being
// attached leaves no art_resized row, and the pictures examined lately in this process
// answer the next arrival without a decode.
func TestALockedItemsOversizedCoverIsExaminedOnce(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	spec := trackSpec{path: "/lib/Al/1.flac", essence: "e1", content: "c1",
		title: "One", artist: "X", albumArt: "X", album: "Al"}
	pid := putTrack(t, st, lib.ID, spec).ItemPID
	if _, err := st.SetItemArt(ctx, pid, model.ArtRoleFront, widePNG(t, 40, 20), "",
		model.Attribution{Source: model.SourceUser}, model.LockOn, false); err != nil {
		t.Fatalf("lock a cover: %v", err)
	}
	raw := widePNG(t, art.MaxSourceDim+200, 300)
	in := trackSpecInput(lib.ID, spec)
	in.CoverArt, in.PreserveLocks = taggedCover(t, raw), true
	if _, err := st.PutScannedTrack(ctx, in); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM art_resized"); n != 0 {
		t.Fatalf("art_resized rows = %d, want none for a cover the lock kept out", n)
	}
	if _, ok := st.examined.get(art.Hash(raw), model.Attribution{}); !ok {
		t.Error("the examined picture was not remembered in the process")
	}
	// A memo in the process answers before any decode: bytes that are not a picture
	// but claim its hash and size come back as the remembered copy.
	claimed := &model.ArtImage{Data: []byte("not the picture"), Hash: art.Hash(raw), Format: "png",
		Width: art.MaxSourceDim + 200, Height: 300, Attribution: model.Attribution{Source: model.SourceTag}}
	again, err := st.examineArt(ctx, claimed)
	if err != nil || again.img.Width != art.MaxSourceDim || again.from != art.Hash(raw) {
		t.Errorf("the second arrival was examined as %dx%d from %q (err %v), want the remembered %d-wide copy",
			again.img.Width, again.img.Height, again.from, err, art.MaxSourceDim)
	}
}
