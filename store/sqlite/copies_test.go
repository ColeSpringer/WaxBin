package sqlite_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/colespringer/waxbin/waxerr"
)

// openCopyStore opens a store over one in-place library rooted in a temp directory. The
// copy rules read a missing file as gone only while its library root is on disk, so
// these tests keep their files under a real root.
func openCopyStore(t *testing.T) (*sqlite.Store, *model.Library, string) {
	t.Helper()
	st, _ := openTestStore(t)
	lib, root := addCopyLibrary(t, st)
	return st, lib, root
}

func addCopyLibrary(t *testing.T, st *sqlite.Store) (*model.Library, string) {
	t.Helper()
	root := t.TempDir()
	lib, err := st.EnsureLibrary(context.Background(), &model.Library{
		Root: []byte(root), DisplayRoot: root, Mode: model.ModeInPlace,
	})
	if err != nil {
		t.Fatalf("ensure library: %v", err)
	}
	return lib, root
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustPut(t *testing.T, st *sqlite.Store, in model.PutScannedTrackInput) *model.ScanItemResult {
	t.Helper()
	r, err := st.PutScannedTrack(context.Background(), in)
	if err != nil {
		t.Fatalf("put %s: %v", in.File.DisplayPath, err)
	}
	return r
}

// rolesOf maps each backing file of an item to its edge role.
func rolesOf(t *testing.T, st *sqlite.Store, item model.PID) map[model.PID]string {
	t.Helper()
	refs, err := st.ItemFiles(context.Background(), item)
	if err != nil {
		t.Fatalf("item files: %v", err)
	}
	out := map[model.PID]string{}
	for _, r := range refs {
		out[r.FilePID] = r.Role
	}
	return out
}

func diagsOf(t *testing.T, st *sqlite.Store, file model.PID, code model.DiagnosticCode) []model.FileDiagnostic {
	t.Helper()
	ds, err := st.FileDiagnostics(context.Background(), model.DiagnosticFilter{FilePID: file, Code: code})
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	return ds
}

func changesAfter(t *testing.T, st *sqlite.Store, seq int64) []model.Change {
	t.Helper()
	cs, err := st.ChangesSince(context.Background(), seq)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	return cs
}

func headSeq(t *testing.T, st *sqlite.Store) int64 {
	t.Helper()
	seq, err := st.LatestChangeSeq(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

// putCopyPair catalogs two files with the same audio and different tags, the first as
// "Original" on "First Album", and returns both results.
func putCopyPair(t *testing.T, st *sqlite.Store, lib *model.Library, p1, p2 string) (*model.ScanItemResult, *model.ScanItemResult) {
	t.Helper()
	touch(t, p1)
	touch(t, p2)
	in1 := input(lib.ID, p1, "sha256:SAME", "sha256:C1", "Original")
	in1.Track.Album = "First Album"
	r1 := mustPut(t, st, in1)
	in2 := input(lib.ID, p2, "sha256:SAME", "sha256:C2", "Retagged")
	in2.Track.Album = "Other Album"
	return r1, mustPut(t, st, in2)
}

func TestCopyAttachesAsAlternate(t *testing.T) {
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	p1, p2 := filepath.Join(root, "a", "1.mp3"), filepath.Join(root, "b", "1.mp3")
	touch(t, p1)
	touch(t, p2)
	in1 := input(lib.ID, p1, "sha256:SAME", "sha256:C1", "Original")
	in1.Track.Album = "First Album"
	r1 := mustPut(t, st, in1)
	seq := headSeq(t, st)

	in2 := input(lib.ID, p2, "sha256:SAME", "sha256:C2", "Retagged")
	in2.Track.Album = "Other Album"
	r2 := mustPut(t, st, in2)
	if !r2.AttachedAsCopy || r2.ItemPID != r1.ItemPID || r2.ItemCreated || !r2.FileCreated {
		t.Fatalf("second put = %+v, want a new file attached as a copy of %s", r2, r1.ItemPID)
	}
	it, err := st.ItemByPID(ctx, r1.ItemPID)
	if err != nil {
		t.Fatal(err)
	}
	if it.Title != "Original" || it.Album != "First Album" || it.FilePID != r1.FilePID {
		t.Errorf("item = %q / %q on %s, want the first file's Original / First Album on %s",
			it.Title, it.Album, it.FilePID, r1.FilePID)
	}
	if got := rolesOf(t, st, r1.ItemPID); len(got) != 2 || got[r1.FilePID] != "primary" || got[r2.FilePID] != "alternate" {
		t.Errorf("edges = %v, want %s primary and %s alternate", got, r1.FilePID, r2.FilePID)
	}
	ds := diagsOf(t, st, r2.FilePID, model.DiagDuplicateCopy)
	if len(ds) != 1 || ds[0].Origin != model.OriginScan || ds[0].Severity != model.SeverityInfo || ds[0].Detail != p1 {
		t.Errorf("copy diagnostics = %+v, want one scan-origin info row naming %s", ds, p1)
	}
	if ds := diagsOf(t, st, r1.FilePID, model.DiagDuplicateCopy); len(ds) != 0 {
		t.Errorf("the primary carries %+v, want nothing", ds)
	}
	var items, files int
	for _, c := range changesAfter(t, st, seq) {
		switch {
		case c.EntityType == "item" && c.EntityPID == r1.ItemPID && c.Op == model.OpUpdate:
			items++
		case c.EntityType == "file" && c.EntityPID == r2.FilePID && c.Op == model.OpCreate:
			files++
		default:
			t.Errorf("unexpected delta %+v", c)
		}
	}
	if items != 1 || files != 1 {
		t.Errorf("deltas: %d item updates and %d file creates, want one of each", items, files)
	}
}

// TestCopyRePutIsSilent: a forced re-read of the copy changes nothing and emits nothing,
// and a retag of the copy moves only its own file row.
func TestCopyRePutIsSilent(t *testing.T) {
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	p1, p2 := filepath.Join(root, "a", "1.mp3"), filepath.Join(root, "b", "1.mp3")
	r1, r2 := putCopyPair(t, st, lib, p1, p2)
	seq := headSeq(t, st)

	in2 := input(lib.ID, p2, "sha256:SAME", "sha256:C2", "Retagged")
	in2.Track.Album = "Other Album"
	again := mustPut(t, st, in2)
	if !again.AttachedAsCopy || again.ItemPID != r1.ItemPID || again.MetadataChanged || again.ContentChanged {
		t.Errorf("re-put = %+v, want the same copy with nothing changed", again)
	}
	if cs := changesAfter(t, st, seq); len(cs) != 0 {
		t.Errorf("re-put deltas = %+v, want none", cs)
	}
	if ds := diagsOf(t, st, r2.FilePID, model.DiagDuplicateCopy); len(ds) != 1 {
		t.Errorf("copy diagnostics after re-put = %+v, want one", ds)
	}

	in2 = input(lib.ID, p2, "sha256:SAME", "sha256:C2b", "Retagged Again")
	retag := mustPut(t, st, in2)
	cs := changesAfter(t, st, seq)
	if !retag.AttachedAsCopy || len(cs) != 1 || cs[0].EntityType != "file" || cs[0].EntityPID != r2.FilePID {
		t.Errorf("retagged copy = %+v with deltas %+v, want one file update", retag, cs)
	}
	if it, err := st.ItemByPID(ctx, r1.ItemPID); err != nil || it.Title != "Original" {
		t.Errorf("item after the copy's retag = %+v (err %v), want Original", it, err)
	}
}

func TestCopyLeavesThePrimaryOwningTheItem(t *testing.T) {
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	p1, p2 := filepath.Join(root, "a", "1.mp3"), filepath.Join(root, "b", "1.mp3")
	r1, r2 := putCopyPair(t, st, lib, p1, p2)

	in1 := input(lib.ID, p1, "sha256:SAME", "sha256:C1b", "Renamed")
	in1.Track.Album = "First Album"
	r := mustPut(t, st, in1)
	if r.AttachedAsCopy || r.ItemPID != r1.ItemPID {
		t.Fatalf("primary re-put = %+v, want the primary of %s", r, r1.ItemPID)
	}
	if it, err := st.ItemByPID(ctx, r1.ItemPID); err != nil || it.Title != "Renamed" {
		t.Errorf("item = %+v (err %v), want the primary's new title Renamed", it, err)
	}
	if got := rolesOf(t, st, r1.ItemPID); got[r1.FilePID] != "primary" || got[r2.FilePID] != "alternate" {
		t.Errorf("edges = %v, want the roles kept", got)
	}
}

// TestCopyRelinkTakesTheGoneRow: a move of the primary relinks its own row, not the
// copy's, now that one essence has several rows.
func TestCopyRelinkTakesTheGoneRow(t *testing.T) {
	st, lib, root := openCopyStore(t)
	p1, p2 := filepath.Join(root, "a", "1.mp3"), filepath.Join(root, "b", "1.mp3")
	r1, r2 := putCopyPair(t, st, lib, p1, p2)

	if err := os.Remove(p1); err != nil {
		t.Fatal(err)
	}
	p3 := filepath.Join(root, "c", "1.mp3")
	touch(t, p3)
	r := mustPut(t, st, input(lib.ID, p3, "sha256:SAME", "sha256:C1", "Original"))
	if !r.Relinked || r.FilePID != r1.FilePID || r.RelinkedFrom != p1 || r.AttachedAsCopy {
		t.Fatalf("moved primary = %+v, want %s relinked from %s", r, r1.FilePID, p1)
	}
	if got := rolesOf(t, st, r1.ItemPID); len(got) != 2 || got[r1.FilePID] != "primary" || got[r2.FilePID] != "alternate" {
		t.Errorf("edges = %v, want the moved primary and the copy", got)
	}
}

func TestCopyRelinksGoneRowsInIDOrder(t *testing.T) {
	st, lib, root := openCopyStore(t)
	p1, p2 := filepath.Join(root, "a", "1.mp3"), filepath.Join(root, "b", "1.mp3")
	r1, r2 := putCopyPair(t, st, lib, p1, p2)
	for _, p := range []string{p1, p2} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	p3, p4 := filepath.Join(root, "c", "1.mp3"), filepath.Join(root, "d", "1.mp3")
	touch(t, p3)
	touch(t, p4)
	first := mustPut(t, st, input(lib.ID, p3, "sha256:SAME", "sha256:C1", "Original"))
	second := mustPut(t, st, input(lib.ID, p4, "sha256:SAME", "sha256:C2", "Retagged"))
	if !first.Relinked || first.FilePID != r1.FilePID || !second.Relinked || second.FilePID != r2.FilePID {
		t.Fatalf("relinks = %s then %s, want %s then %s", first.FilePID, second.FilePID, r1.FilePID, r2.FilePID)
	}
	if !second.AttachedAsCopy {
		t.Errorf("second relink = %+v, want it to stay the copy", second)
	}
	if got := rolesOf(t, st, r1.ItemPID); len(got) != 2 || got[r1.FilePID] != "primary" || got[r2.FilePID] != "alternate" {
		t.Errorf("edges = %v, want the roles kept across the moves", got)
	}
}

// TestCopyAcrossLibrariesAttaches: a copy in another library, read-only or not, joins
// the item rather than starting one; the relink lookup stays inside one library.
func TestCopyAcrossLibrariesAttaches(t *testing.T) {
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	p1 := filepath.Join(root, "a", "1.mp3")
	touch(t, p1)
	r1 := mustPut(t, st, input(lib.ID, p1, "sha256:SAME", "sha256:C1", "Original"))

	other, otherRoot := addCopyLibrary(t, st)
	ro, roRoot := addCopyLibrary(t, st)
	if _, err := st.SetLibraryReadOnly(ctx, ro.PID, true); err != nil {
		t.Fatal(err)
	}
	p2, p3 := filepath.Join(otherRoot, "1.mp3"), filepath.Join(roRoot, "1.mp3")
	touch(t, p2)
	touch(t, p3)
	r2 := mustPut(t, st, input(other.ID, p2, "sha256:SAME", "sha256:C2", "Elsewhere"))
	r3 := mustPut(t, st, input(ro.ID, p3, "sha256:SAME", "sha256:C3", "Archived"))
	for _, r := range []*model.ScanItemResult{r2, r3} {
		if !r.AttachedAsCopy || r.ItemPID != r1.ItemPID || !r.FileCreated || r.Relinked {
			t.Errorf("copy = %+v, want a new file attached to %s", r, r1.ItemPID)
		}
	}
	got := rolesOf(t, st, r1.ItemPID)
	if len(got) != 3 || got[r1.FilePID] != "primary" || got[r2.FilePID] != "alternate" || got[r3.FilePID] != "alternate" {
		t.Errorf("edges = %v, want one primary and two alternates", got)
	}
	if it, err := st.ItemByPID(ctx, r1.ItemPID); err != nil || it.Title != "Original" || it.LibraryPID != lib.PID {
		t.Errorf("item = %+v (err %v), want Original in the primary's library", it, err)
	}
}

// TestCopyEncodingRanksByQuality: two encodings under one recording MBID share an item,
// and the better one is its primary whichever arrives first, its tags owning the item.
func TestCopyEncodingRanksByQuality(t *testing.T) {
	encoding := func(lib *model.Library, path, essence, title, codec string, bitrate, depth int) model.PutScannedTrackInput {
		in := input(lib.ID, path, essence, "sha256:C"+essence, title)
		in.Item.IdentityKey = "mbid:rec-1"
		in.Track.MBID = "rec-1"
		in.File.Codec, in.File.Bitrate, in.File.SampleRate, in.File.BitDepth = codec, bitrate, 44100, depth
		return in
	}
	for _, flacFirst := range []bool{false, true} {
		name := "mp3 first"
		if flacFirst {
			name = "flac first"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			st, lib, root := openCopyStore(t)
			pm, pf := filepath.Join(root, "mp3", "1.mp3"), filepath.Join(root, "flac", "1.flac")
			touch(t, pm)
			touch(t, pf)
			mp3 := encoding(lib, pm, "EMP3", "MP3 Title", "mp3", 320000, 0)
			flac := encoding(lib, pf, "EFLAC", "FLAC Title", "flac", 900000, 16)
			var rm, rf *model.ScanItemResult
			if flacFirst {
				rf = mustPut(t, st, flac)
				rm = mustPut(t, st, mp3)
				if !rm.AttachedAsCopy || rm.ItemPID != rf.ItemPID {
					t.Errorf("mp3 after flac = %+v, want it attached to %s", rm, rf.ItemPID)
				}
			} else {
				rm = mustPut(t, st, mp3)
				seq := headSeq(t, st)
				rf = mustPut(t, st, flac)
				if rf.AttachedAsCopy || rf.ItemPID != rm.ItemPID || rf.ItemCreated {
					t.Errorf("flac after mp3 = %+v, want it to take over %s", rf, rm.ItemPID)
				}
				var updated bool
				for _, c := range changesAfter(t, st, seq) {
					updated = updated || (c.EntityType == "item" && c.EntityPID == rm.ItemPID && c.Op == model.OpUpdate)
				}
				if !updated {
					t.Error("the take-over emitted no item update")
				}
			}
			it, err := st.ItemByPID(ctx, rm.ItemPID)
			if err != nil {
				t.Fatal(err)
			}
			if it.Title != "FLAC Title" || it.FilePID != rf.FilePID {
				t.Errorf("item = %q on %s, want FLAC Title on %s", it.Title, it.FilePID, rf.FilePID)
			}
			if got := rolesOf(t, st, rm.ItemPID); len(got) != 2 || got[rf.FilePID] != "primary" || got[rm.FilePID] != "alternate" {
				t.Errorf("edges = %v, want the flac primary and the mp3 alternate", got)
			}
			if ds := diagsOf(t, st, rm.FilePID, model.DiagAlternateEncoding); len(ds) != 1 || ds[0].Detail != pf || ds[0].Severity != model.SeverityInfo {
				t.Errorf("mp3 diagnostics = %+v, want one alternate_encoding row naming %s", ds, pf)
			}
			if ds := diagsOf(t, st, rf.FilePID, model.DiagAlternateEncoding); len(ds) != 0 {
				t.Errorf("flac diagnostics = %+v, want none", ds)
			}
			for _, f := range []model.PID{rm.FilePID, rf.FilePID} {
				if ds := diagsOf(t, st, f, model.DiagDuplicateCopy); len(ds) != 0 {
					t.Errorf("duplicate_copy on %s = %+v, want none for another encoding", f, ds)
				}
			}

			// A tie keeps the standing primary: an encoding no better than it is an alternate.
			pt := filepath.Join(root, "tie", "1.flac")
			touch(t, pt)
			rt := mustPut(t, st, encoding(lib, pt, "ETIE", "Tie Title", "flac", 900000, 16))
			if !rt.AttachedAsCopy {
				t.Errorf("equal encoding = %+v, want an alternate", rt)
			}
			if it, err := st.ItemByPID(ctx, rm.ItemPID); err != nil || it.FilePID != rf.FilePID {
				t.Errorf("item after a tie = %+v (err %v), want the flac kept", it, err)
			}
		})
	}
}

// TestCopyWaitsForReconciliationToReplaceAGonePrimary: a copy re-read while its primary's
// path is missing stays the alternate, since mid-walk a missing path may be a primary moved
// to a folder not reached yet; reconciling the gone file is what promotes the copy.
func TestCopyWaitsForReconciliationToReplaceAGonePrimary(t *testing.T) {
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	p1, p2 := filepath.Join(root, "a", "1.mp3"), filepath.Join(root, "b", "1.mp3")
	r1, r2 := putCopyPair(t, st, lib, p1, p2)
	if err := os.Remove(p1); err != nil {
		t.Fatal(err)
	}
	in2 := input(lib.ID, p2, "sha256:SAME", "sha256:C2", "Retagged")
	in2.Track.Album = "Other Album"
	if r := mustPut(t, st, in2); !r.AttachedAsCopy || r.ItemPID != r1.ItemPID {
		t.Fatalf("re-read copy = %+v, want it kept the alternate of %s", r, r1.ItemPID)
	}
	if it, err := st.ItemByPID(ctx, r1.ItemPID); err != nil || it.Title != "Original" || it.FilePID != r1.FilePID {
		t.Fatalf("item = %+v (err %v), want the primary kept until reconciliation", it, err)
	}
	res, err := st.MarkFilesMissing(ctx, []model.PID{r1.FilePID})
	if err != nil || len(res.Promoted) != 1 || res.Promoted[0].FilePID != r2.FilePID {
		t.Fatalf("reconcile = %+v (err %v), want the copy promoted", res, err)
	}
	if got := rolesOf(t, st, r1.ItemPID); len(got) != 1 || got[r2.FilePID] != "primary" {
		t.Errorf("edges = %v, want the copy alone as primary", got)
	}
}

// TestCopyRelinkKeepsEachRowWithItsItem: two items with the same audio under different
// keys (one untagged, one carrying a recording id), both files moved, the tagged one walked
// first: each move relinks its own row, and neither item is lost.
func TestCopyRelinkKeepsEachRowWithItsItem(t *testing.T) {
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	pb, pa := filepath.Join(root, "b", "1.mp3"), filepath.Join(root, "a", "1.mp3")
	touch(t, pb)
	touch(t, pa)
	inB := input(lib.ID, pb, "sha256:SAME", "sha256:CB", "Untagged")
	inA := input(lib.ID, pa, "sha256:SAME", "sha256:CA", "Tagged")
	inA.Item.IdentityKey = "mbid:rec-a"
	rb, ra := mustPut(t, st, inB), mustPut(t, st, inA)
	if rb.ItemPID == ra.ItemPID {
		t.Fatalf("one item for two keys: %s", rb.ItemPID)
	}
	for _, p := range []string{pa, pb} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	pa2, pb2 := filepath.Join(root, "moved", "a.mp3"), filepath.Join(root, "moved", "b.mp3")
	touch(t, pa2)
	touch(t, pb2)
	inA.File.Path, inA.File.DisplayPath = []byte(pa2), pa2
	inB.File.Path, inB.File.DisplayPath = []byte(pb2), pb2
	if r := mustPut(t, st, inA); !r.Relinked || r.FilePID != ra.FilePID || r.ItemPID != ra.ItemPID {
		t.Errorf("moved tagged file = %+v, want its own row %s relinked", r, ra.FilePID)
	}
	if r := mustPut(t, st, inB); !r.Relinked || r.FilePID != rb.FilePID || r.ItemPID != rb.ItemPID {
		t.Errorf("moved untagged file = %+v, want its own row %s relinked", r, rb.FilePID)
	}
	for _, pid := range []model.PID{ra.ItemPID, rb.ItemPID} {
		if _, err := st.ItemByPID(ctx, pid); err != nil {
			t.Errorf("item %s: %v, want it kept", pid, err)
		}
	}
}

// TestCopyRelinkLeavesAnotherItemsGoneRow: a move never takes the gone row of another item,
// which reconciliation then marks missing rather than deleting.
func TestCopyRelinkLeavesAnotherItemsGoneRow(t *testing.T) {
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	pb, pa := filepath.Join(root, "b", "1.mp3"), filepath.Join(root, "a", "1.mp3")
	touch(t, pb)
	touch(t, pa)
	inB := input(lib.ID, pb, "sha256:SAME", "sha256:CB", "Untagged")
	inA := input(lib.ID, pa, "sha256:SAME", "sha256:CA2", "Tagged Again")
	inA.Item.IdentityKey = "mbid:rec-a"
	rb, ra := mustPut(t, st, inB), mustPut(t, st, inA)
	for _, p := range []string{pa, pb} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	pa2 := filepath.Join(root, "moved", "a.mp3")
	touch(t, pa2)
	// The tagged file moved and retagged: no row holds its bytes, and only its own row
	// backs its item.
	inA.File.Path, inA.File.DisplayPath, inA.File.ContentHash = []byte(pa2), pa2, "sha256:CA3"
	if r := mustPut(t, st, inA); !r.Relinked || r.FilePID != ra.FilePID {
		t.Errorf("moved tagged file = %+v, want its own row %s relinked", r, ra.FilePID)
	}
	if _, err := st.MarkFilesMissing(ctx, []model.PID{rb.FilePID}); err != nil {
		t.Fatal(err)
	}
	if it, err := st.ItemByPID(ctx, rb.ItemPID); err != nil || it.State != model.StateMissing {
		t.Errorf("untagged item = %+v (err %v), want it kept and missing", it, err)
	}
}

// TestRelinkFollowsAKeyChange: a file retagged under a key no item holds yet (a recording
// id added) and moved in the same pass relinks its own row, the only gone row of its
// audio, so the outcome matches retagging it in place and then moving it: the file keeps
// its pid under the new key and the old item, left with no file, goes.
func TestRelinkFollowsAKeyChange(t *testing.T) {
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	p1, p2 := filepath.Join(root, "a", "1.mp3"), filepath.Join(root, "b", "1.mp3")
	touch(t, p1)
	r1 := mustPut(t, st, input(lib.ID, p1, "sha256:SAME", "sha256:C1", "Song"))
	if err := os.Remove(p1); err != nil {
		t.Fatal(err)
	}
	touch(t, p2)
	in := input(lib.ID, p2, "sha256:SAME", "sha256:C2", "Song")
	in.Item.IdentityKey, in.Track.MBID = "mbid:rec-new", "rec-new"
	r2 := mustPut(t, st, in)
	if !r2.Relinked || r2.FilePID != r1.FilePID {
		t.Errorf("moved and retagged file = %+v, want its row %s relinked", r2, r1.FilePID)
	}
	if _, err := st.ItemByPID(ctx, r1.ItemPID); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("old item err = %v, want it gone with its file", err)
	}
}

// TestCopyMovedWithItsOriginalKeepsTheRoles: an item's original and its retagged copy moved
// together, the copy walked first: the copy relinks its own row and stays the alternate, so
// the item keeps the original's tags.
func TestCopyMovedWithItsOriginalKeepsTheRoles(t *testing.T) {
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	p1, p2 := filepath.Join(root, "a", "1.mp3"), filepath.Join(root, "b", "1.mp3")
	r1, r2 := putCopyPair(t, st, lib, p1, p2)
	for _, p := range []string{p1, p2} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	c2, c1 := filepath.Join(root, "new", "b1.mp3"), filepath.Join(root, "new", "z1.mp3")
	touch(t, c2)
	touch(t, c1)
	in2 := input(lib.ID, c2, "sha256:SAME", "sha256:C2", "Retagged")
	in2.Track.Album = "Other Album"
	if r := mustPut(t, st, in2); r.FilePID != r2.FilePID || !r.AttachedAsCopy {
		t.Errorf("moved copy = %+v, want its own row %s kept the alternate", r, r2.FilePID)
	}
	in1 := input(lib.ID, c1, "sha256:SAME", "sha256:C1", "Original")
	in1.Track.Album = "First Album"
	if r := mustPut(t, st, in1); r.FilePID != r1.FilePID || r.AttachedAsCopy {
		t.Errorf("moved original = %+v, want its own row %s kept the primary", r, r1.FilePID)
	}
	if it, err := st.ItemByPID(ctx, r1.ItemPID); err != nil || it.Title != "Original" || it.FilePID != r1.FilePID {
		t.Errorf("item = %+v (err %v), want the original's Original", it, err)
	}
}

// TestCopyKeepsAPrimaryUnderAnAbsentRoot: a primary whose whole library root is missing
// may only be unmounted, so a copy elsewhere stays an alternate.
func TestCopyKeepsAPrimaryUnderAnAbsentRoot(t *testing.T) {
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	p1 := filepath.Join(root, "a", "1.mp3")
	touch(t, p1)
	r1 := mustPut(t, st, input(lib.ID, p1, "sha256:SAME", "sha256:C1", "Original"))
	other, otherRoot := addCopyLibrary(t, st)
	p2 := filepath.Join(otherRoot, "1.mp3")
	touch(t, p2)
	mustPut(t, st, input(other.ID, p2, "sha256:SAME", "sha256:C2", "Elsewhere"))
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	r := mustPut(t, st, input(other.ID, p2, "sha256:SAME", "sha256:C2", "Elsewhere"))
	if !r.AttachedAsCopy {
		t.Errorf("copy with the primary's root unmounted = %+v, want it kept an alternate", r)
	}
	if it, err := st.ItemByPID(ctx, r1.ItemPID); err != nil || it.FilePID != r1.FilePID || it.Title != "Original" {
		t.Errorf("item = %+v (err %v), want the original primary kept", it, err)
	}
}

// TestCopyKeepsTheItemAcrossAnEssenceRekey: when the essence algorithm changes, a copy
// re-read first re-keys its item in place, as a primary does, so its item is neither
// forked nor left to be deleted when the primary follows.
func TestCopyKeepsTheItemAcrossAnEssenceRekey(t *testing.T) {
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	p1, p2 := filepath.Join(root, "a", "1.mp3"), filepath.Join(root, "b", "1.mp3")
	r1, r2 := putCopyPair(t, st, lib, p1, p2)

	in2 := input(lib.ID, p2, "sha256:NEWALGO", "sha256:C2", "Retagged")
	in2.Track.Album = "Other Album"
	a := mustPut(t, st, in2)
	b := mustPut(t, st, input(lib.ID, p1, "sha256:NEWALGO", "sha256:C1", "Original"))
	if a.ItemPID != r1.ItemPID || b.ItemPID != r1.ItemPID || !a.AttachedAsCopy || b.AttachedAsCopy {
		t.Fatalf("re-reads = %+v then %+v, want both on %s with the copy still the alternate", a, b, r1.ItemPID)
	}
	if got := rolesOf(t, st, r1.ItemPID); len(got) != 2 || got[r1.FilePID] != "primary" || got[r2.FilePID] != "alternate" {
		t.Errorf("edges = %v, want the roles kept", got)
	}
	if it, err := st.ItemByPID(ctx, r1.ItemPID); err != nil || it.Title != "Original" {
		t.Errorf("item = %+v (err %v), want Original", it, err)
	}
}

func promotedPIDs(ps []model.PromotedFile) []model.PID {
	var out []model.PID
	for _, p := range ps {
		out = append(out, p.FilePID)
	}
	return out
}

// TestCopyPromotedWhenThePrimaryIsTrashed: trashing the primary promotes the copy, whose
// copy diagnostic goes, and reports it for a re-read; the item stays present.
func TestCopyPromotedWhenThePrimaryIsTrashed(t *testing.T) {
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	p1, p2 := filepath.Join(root, "a", "1.mp3"), filepath.Join(root, "b", "1.mp3")
	r1, r2 := putCopyPair(t, st, lib, p1, p2)

	res, err := st.TrashFile(ctx, model.TrashFileInput{FilePID: r1.FilePID, TrashPath: []byte(p1 + ".trash"), TrashDisplay: p1 + ".trash"})
	if err != nil {
		t.Fatal(err)
	}
	if res.TrashPID == "" || len(res.Promoted) != 1 || res.Promoted[0].FilePID != r2.FilePID ||
		string(res.Promoted[0].Path) != p2 || res.Promoted[0].LibraryID != lib.ID {
		t.Fatalf("trash = %+v, want an entry and %s promoted at %s", res, r2.FilePID, p2)
	}
	it, err := st.ItemByPID(ctx, r1.ItemPID)
	if err != nil || it.State != model.StatePresent || it.FilePID != r2.FilePID {
		t.Fatalf("item = %+v (err %v), want present on the copy", it, err)
	}
	if got := rolesOf(t, st, r1.ItemPID); len(got) != 1 || got[r2.FilePID] != "primary" {
		t.Errorf("edges = %v, want the copy alone as primary", got)
	}
	if ds := diagsOf(t, st, r2.FilePID, model.DiagDuplicateCopy); len(ds) != 0 {
		t.Errorf("promoted copy keeps %+v, want its copy diagnostic gone", ds)
	}
}

func TestCopyTrashedLeavesTheItemAlone(t *testing.T) {
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	p1, p2 := filepath.Join(root, "a", "1.mp3"), filepath.Join(root, "b", "1.mp3")
	r1, r2 := putCopyPair(t, st, lib, p1, p2)
	res, err := st.TrashFile(ctx, model.TrashFileInput{FilePID: r2.FilePID, TrashPath: []byte(p2 + ".trash"), TrashDisplay: p2 + ".trash"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Promoted) != 0 {
		t.Errorf("promoted = %v, want none", promotedPIDs(res.Promoted))
	}
	it, err := st.ItemByPID(ctx, r1.ItemPID)
	if err != nil || it.FilePID != r1.FilePID || it.Title != "Original" || it.State != model.StatePresent {
		t.Errorf("item = %+v (err %v), want the primary untouched", it, err)
	}
}

// TestCopyPromotionPrefersAWritableLibrary is the plan's first review focus: with the
// primary in a writable library, the lowest-id alternate in a read-only one and another
// in a writable one, losing the primary promotes the writable copy, and a better
// encoding outranks a lesser one in the same standing.
func TestCopyPromotionPrefersAWritableLibrary(t *testing.T) {
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	ro, roRoot := addCopyLibrary(t, st)
	if _, err := st.SetLibraryReadOnly(ctx, ro.PID, true); err != nil {
		t.Fatal(err)
	}
	other, otherRoot := addCopyLibrary(t, st)
	p1, pr, pw := filepath.Join(root, "1.mp3"), filepath.Join(roRoot, "1.mp3"), filepath.Join(otherRoot, "1.mp3")
	for _, p := range []string{p1, pr, pw} {
		touch(t, p)
	}
	r1 := mustPut(t, st, input(lib.ID, p1, "sha256:SAME", "sha256:C1", "Original"))
	rr := mustPut(t, st, input(ro.ID, pr, "sha256:SAME", "sha256:CR", "Archive"))
	rw := mustPut(t, st, input(other.ID, pw, "sha256:SAME", "sha256:CW", "Working"))
	res, err := st.DetachFile(ctx, r1.FilePID)
	if err != nil {
		t.Fatal(err)
	}
	if got := promotedPIDs(res.Promoted); len(got) != 1 || got[0] != rw.FilePID {
		t.Fatalf("promoted = %v, want the writable copy %s over the read-only %s", got, rw.FilePID, rr.FilePID)
	}
	if it, err := st.ItemByPID(ctx, r1.ItemPID); err != nil || it.LibraryPID != other.PID {
		t.Errorf("item = %+v (err %v), want it in the writable library", it, err)
	}

	encoding := func(path, essence, codec string, bitrate, depth int) model.PutScannedTrackInput {
		in := input(lib.ID, path, essence, "sha256:C"+essence, essence)
		in.Item.IdentityKey = "mbid:rec-2"
		in.File.Codec, in.File.Bitrate, in.File.SampleRate, in.File.BitDepth = codec, bitrate, 44100, depth
		return in
	}
	pa, pb, pc := filepath.Join(root, "x.flac"), filepath.Join(root, "y.mp3"), filepath.Join(root, "z.flac")
	for _, p := range []string{pa, pb, pc} {
		touch(t, p)
	}
	ra := mustPut(t, st, encoding(pa, "EA", "flac", 900000, 24))
	mustPut(t, st, encoding(pb, "EB", "mp3", 320000, 0))
	rc := mustPut(t, st, encoding(pc, "EC", "flac", 900000, 16))
	res, err = st.DetachFile(ctx, ra.FilePID)
	if err != nil {
		t.Fatal(err)
	}
	if got := promotedPIDs(res.Promoted); len(got) != 1 || got[0] != rc.FilePID {
		t.Errorf("promoted = %v, want the 16-bit flac %s over the mp3", got, rc.FilePID)
	}
}

// TestEncodingTakeoverIgnoresWalkOrder: a better encoding read while the primary was
// moving (its old path gone, its new one not walked yet) waits as an alternate, and takes
// the item over when the primary is read at its new path, the end state the other order
// reaches. The promoted file is reported for a re-read.
func TestEncodingTakeoverIgnoresWalkOrder(t *testing.T) {
	st, lib, root := openCopyStore(t)
	encoding := func(path, essence, title, codec string, bitrate, depth int) model.PutScannedTrackInput {
		touch(t, path)
		in := input(lib.ID, path, essence, "sha256:C"+essence, title)
		in.Item.IdentityKey, in.Track.MBID = "mbid:rec-5", "rec-5"
		in.File.Codec, in.File.Bitrate, in.File.SampleRate, in.File.BitDepth = codec, bitrate, 44100, depth
		return in
	}
	p1, moved, pf := filepath.Join(root, "a", "1.mp3"), filepath.Join(root, "b", "1.mp3"), filepath.Join(root, "f", "1.flac")
	mp3 := encoding(p1, "EMP3", "MP3 Title", "mp3", 320000, 0)
	rm := mustPut(t, st, mp3)
	if err := os.Remove(p1); err != nil {
		t.Fatal(err)
	}
	rf := mustPut(t, st, encoding(pf, "EFLAC", "FLAC Title", "flac", 900000, 16))
	if !rf.AttachedAsCopy {
		t.Fatalf("flac beside a moving primary = %+v, want it waiting as an alternate", rf)
	}
	touch(t, moved)
	mp3.File.Path, mp3.File.DisplayPath = []byte(moved), moved
	r := mustPut(t, st, mp3)
	if !r.Relinked || !r.AttachedAsCopy {
		t.Errorf("mp3 at its new path = %+v, want it relinked and yielding to the flac", r)
	}
	if got := rolesOf(t, st, rm.ItemPID); got[rf.FilePID] != "primary" || got[rm.FilePID] != "alternate" {
		t.Errorf("edges = %v, want the flac primary and the mp3 alternate", got)
	}
	if got := promotedPIDs(r.Promoted); len(got) != 1 || got[0] != rf.FilePID {
		t.Errorf("promoted = %v, want the flac for a re-read", got)
	}
	if ds := diagsOf(t, st, rm.FilePID, model.DiagAlternateEncoding); len(ds) != 1 || ds[0].Detail != pf {
		t.Errorf("mp3 diagnostics = %+v, want alternate_encoding naming %s", ds, pf)
	}
}

// TestCopyEncodingInAReadOnlyLibraryAttaches: a better encoding in a read-only library
// attaches to an item whose primary is in a writable one, and a writable encoding takes
// an item over from a read-only primary whatever its quality, the order promotion uses.
func TestCopyEncodingInAReadOnlyLibraryAttaches(t *testing.T) {
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	ro, roRoot := addCopyLibrary(t, st)
	if _, err := st.SetLibraryReadOnly(ctx, ro.PID, true); err != nil {
		t.Fatal(err)
	}
	encoding := func(libID int64, path, key, essence, title, codec string, bitrate, depth int) model.PutScannedTrackInput {
		in := input(libID, path, essence, "sha256:C"+essence, title)
		in.Item.IdentityKey = key
		in.File.Codec, in.File.Bitrate, in.File.SampleRate, in.File.BitDepth = codec, bitrate, 44100, depth
		return in
	}
	pm, pf := filepath.Join(root, "1.mp3"), filepath.Join(roRoot, "1.flac")
	touch(t, pm)
	touch(t, pf)
	rm := mustPut(t, st, encoding(lib.ID, pm, "mbid:rec-3", "EMP3", "Writable", "mp3", 320000, 0))
	rf := mustPut(t, st, encoding(ro.ID, pf, "mbid:rec-3", "EFLAC", "Archive", "flac", 900000, 24))
	if !rf.AttachedAsCopy || rf.ItemPID != rm.ItemPID {
		t.Errorf("read-only flac = %+v, want it attached to %s", rf, rm.ItemPID)
	}
	it, err := st.ItemByPID(ctx, rm.ItemPID)
	if err != nil {
		t.Fatal(err)
	}
	if it.Title != "Writable" || it.FilePID != rm.FilePID || it.LibraryPID != lib.PID {
		t.Errorf("item = %q on %s in %s, want Writable on the mp3 in %s", it.Title, it.FilePID, it.LibraryPID, lib.PID)
	}
	if ds := diagsOf(t, st, rf.FilePID, model.DiagAlternateEncoding); len(ds) != 1 || ds[0].Detail != pm {
		t.Errorf("flac diagnostics = %+v, want one alternate_encoding row naming %s", ds, pm)
	}

	pa, pb := filepath.Join(roRoot, "2.flac"), filepath.Join(root, "2.mp3")
	touch(t, pa)
	touch(t, pb)
	ra := mustPut(t, st, encoding(ro.ID, pa, "mbid:rec-4", "EFLAC4", "Archive", "flac", 900000, 24))
	rb := mustPut(t, st, encoding(lib.ID, pb, "mbid:rec-4", "EMP34", "Writable", "mp3", 128000, 0))
	if rb.AttachedAsCopy || rb.ItemPID != ra.ItemPID {
		t.Errorf("writable mp3 = %+v, want it to take over %s", rb, ra.ItemPID)
	}
	if got := rolesOf(t, st, ra.ItemPID); got[rb.FilePID] != "primary" || got[ra.FilePID] != "alternate" {
		t.Errorf("edges = %v, want the writable mp3 primary and the read-only flac alternate", got)
	}
}

// TestMarkFilesMissingPromotesACopy: reconciling a vanished primary whose copy is on
// disk promotes the copy rather than marking the item missing, and drops the gone row.
func TestMarkFilesMissingPromotesACopy(t *testing.T) {
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	p1, p2 := filepath.Join(root, "a", "1.mp3"), filepath.Join(root, "b", "1.mp3")
	r1, r2 := putCopyPair(t, st, lib, p1, p2)
	if err := os.Remove(p1); err != nil {
		t.Fatal(err)
	}
	res, err := st.MarkFilesMissing(ctx, []model.PID{r1.FilePID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Marked != 0 || res.Dropped != 0 || len(res.Promoted) != 1 || res.Promoted[0].FilePID != r2.FilePID {
		t.Fatalf("reconcile = %+v, want the copy promoted", res)
	}
	if it, err := st.ItemByPID(ctx, r1.ItemPID); err != nil || it.State != model.StatePresent || it.FilePID != r2.FilePID {
		t.Errorf("item = %+v (err %v), want present on the copy", it, err)
	}
	if _, err := st.FileByPath(ctx, []byte(p1)); err == nil {
		t.Error("the vanished primary's row is still cataloged")
	}
}

// TestCopyPromotionPrefersACopyOnDisk: a copy under an absent library root (an unplugged
// drive) loses to one on disk, whichever attached first, both when the primary is
// trashed and when reconciliation finds it gone.
func TestCopyPromotionPrefersACopyOnDisk(t *testing.T) {
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	unplugged, offRoot := addCopyLibrary(t, st)
	other, onRoot := addCopyLibrary(t, st)
	p1, q1 := filepath.Join(root, "1.mp3"), filepath.Join(root, "2.mp3")
	pb, qb := filepath.Join(offRoot, "1.mp3"), filepath.Join(offRoot, "2.mp3")
	pc, qc := filepath.Join(onRoot, "1.mp3"), filepath.Join(onRoot, "2.mp3")
	for _, p := range []string{p1, q1, pb, qb, pc, qc} {
		touch(t, p)
	}
	r1 := mustPut(t, st, input(lib.ID, p1, "sha256:ONE", "sha256:C1", "One"))
	mustPut(t, st, input(unplugged.ID, pb, "sha256:ONE", "sha256:CB", "One"))
	rc := mustPut(t, st, input(other.ID, pc, "sha256:ONE", "sha256:CC", "One"))
	s1 := mustPut(t, st, input(lib.ID, q1, "sha256:TWO", "sha256:D1", "Two"))
	mustPut(t, st, input(unplugged.ID, qb, "sha256:TWO", "sha256:DB", "Two"))
	sc := mustPut(t, st, input(other.ID, qc, "sha256:TWO", "sha256:DC", "Two"))
	if err := os.RemoveAll(offRoot); err != nil {
		t.Fatal(err)
	}

	res, err := st.DetachFile(ctx, r1.FilePID)
	if err != nil {
		t.Fatal(err)
	}
	if got := promotedPIDs(res.Promoted); len(got) != 1 || got[0] != rc.FilePID {
		t.Errorf("trash promoted %v, want the copy on disk %s", got, rc.FilePID)
	}
	if err := os.Remove(q1); err != nil {
		t.Fatal(err)
	}
	mr, err := st.MarkFilesMissing(ctx, []model.PID{s1.FilePID})
	if err != nil {
		t.Fatal(err)
	}
	if got := promotedPIDs(mr.Promoted); len(got) != 1 || got[0] != sc.FilePID {
		t.Errorf("reconcile promoted %v, want the copy on disk %s", got, sc.FilePID)
	}
}

// TestCopyGoneStillGivesTheItemAPrimary: trashing the primary of an item whose only copy
// is gone from disk promotes that copy anyway, so the item keeps a primary, and marks the
// item missing rather than leaving it present with no file. A reconciliation never
// promotes a gone copy in place of a gone primary.
func TestCopyGoneStillGivesTheItemAPrimary(t *testing.T) {
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	p1, p2 := filepath.Join(root, "a", "1.mp3"), filepath.Join(root, "b", "1.mp3")
	r1, r2 := putCopyPair(t, st, lib, p1, p2)
	if err := os.Remove(p2); err != nil {
		t.Fatal(err)
	}
	res, err := st.DetachFile(ctx, r1.FilePID)
	if err != nil {
		t.Fatal(err)
	}
	if got := promotedPIDs(res.Promoted); len(got) != 1 || got[0] != r2.FilePID {
		t.Errorf("promoted = %v, want the gone copy %s", got, r2.FilePID)
	}
	if it, err := st.ItemByPID(ctx, r1.ItemPID); err != nil || it.FilePID != r2.FilePID || it.State != model.StateMissing {
		t.Fatalf("item = %+v (err %v), want it missing on the gone copy", it, err)
	}
	var mr *model.MissingResult

	q1, q2 := filepath.Join(root, "c", "2.mp3"), filepath.Join(root, "d", "2.mp3")
	touch(t, q1)
	touch(t, q2)
	s1 := mustPut(t, st, input(lib.ID, q1, "sha256:OTHER", "sha256:E1", "Other"))
	other, otherRoot := addCopyLibrary(t, st)
	q3 := filepath.Join(otherRoot, "2.mp3")
	touch(t, q3)
	s3 := mustPut(t, st, input(other.ID, q3, "sha256:OTHER", "sha256:E3", "Other"))
	for _, p := range []string{q1, q3} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	mr, err = st.MarkFilesMissing(ctx, []model.PID{s1.FilePID})
	if err != nil {
		t.Fatal(err)
	}
	if len(mr.Promoted) != 0 {
		t.Errorf("reconcile promoted %v, want the gone copy %s left an alternate", promotedPIDs(mr.Promoted), s3.FilePID)
	}
	if got := rolesOf(t, st, s1.ItemPID); got[s1.FilePID] != "primary" || got[s3.FilePID] != "alternate" {
		t.Errorf("edges = %v, want the gone primary kept beside the gone copy", got)
	}
}

// TestCopyDiagnosticFollowsAMovedPrimary: a copy's diagnostic names its primary's
// current path after the primary moves, by a rescan's relink, an organize move, or a
// relocated library root.
func TestCopyDiagnosticFollowsAMovedPrimary(t *testing.T) {
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	p1, p2 := filepath.Join(root, "a", "1.mp3"), filepath.Join(root, "b", "1.mp3")
	r1, r2 := putCopyPair(t, st, lib, p1, p2)
	detail := func(want, step string) {
		t.Helper()
		if ds := diagsOf(t, st, r2.FilePID, model.DiagDuplicateCopy); len(ds) != 1 || ds[0].Detail != want {
			t.Errorf("after %s the copy's diagnostics = %+v, want one naming %s", step, ds, want)
		}
	}
	detail(p1, "the attach")

	moved := filepath.Join(root, "c", "1.mp3")
	touch(t, moved)
	if err := os.Remove(p1); err != nil {
		t.Fatal(err)
	}
	in := input(lib.ID, moved, "sha256:SAME", "sha256:C1", "Original")
	in.Track.Album = "First Album"
	if res := mustPut(t, st, in); !res.Relinked {
		t.Fatalf("re-put = %+v, want a relink", res)
	}
	detail(moved, "a relink")

	organized := filepath.Join(root, "d", "1.mp3")
	move := model.RelocateInput{FilePID: r1.FilePID, JobPID: model.NewPID(), SrcPath: []byte(moved),
		NewPath: []byte(organized), NewDisplayPath: organized, NewRelPath: []byte("d/1.mp3")}
	jpid, err := st.PlanMove(ctx, move)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CommitMove(ctx, jpid, move); err != nil {
		t.Fatal(err)
	}
	detail(organized, "an organize move")

	newRoot := t.TempDir()
	if err := st.RelocateLibraryRoot(ctx, lib.PID, newRoot); err != nil {
		t.Fatal(err)
	}
	detail(filepath.Join(newRoot, "d", "1.mp3"), "a relocated root")
}

// stateOf returns an item's state.
func stateOf(t *testing.T, st *sqlite.Store, pid model.PID) model.ItemState {
	t.Helper()
	it, err := st.ItemByPID(context.Background(), pid)
	if err != nil {
		t.Fatal(err)
	}
	return it.State
}

// TestCopyRevivesAMissingItem: a copy arriving for an item reconciliation marked missing
// brings it back (a file moved to another library, whose own scan ran first). With the
// primary gone the copy takes its place (the gone file stays as an alternate for the next
// reconciliation); with the primary back on disk the copy attaches and the item is present
// again. Either way the item emits an update.
func TestCopyRevivesAMissingItem(t *testing.T) {
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	other, otherRoot := addCopyLibrary(t, st)
	p1, p2 := filepath.Join(root, "a", "1.mp3"), filepath.Join(otherRoot, "b", "1.mp3")
	touch(t, p1)
	in1 := input(lib.ID, p1, "sha256:SAME", "sha256:C1", "Original")
	r1 := mustPut(t, st, in1)
	if err := os.Remove(p1); err != nil {
		t.Fatal(err)
	}
	if mr, err := st.MarkFilesMissing(ctx, []model.PID{r1.FilePID}); err != nil || mr.Marked != 1 {
		t.Fatalf("reconcile = %+v (err %v), want the item missing", mr, err)
	}
	touch(t, p2)
	seq := headSeq(t, st)
	r2 := mustPut(t, st, input(other.ID, p2, "sha256:SAME", "sha256:C2", "Copy"))
	if r2.ItemPID != r1.ItemPID || r2.AttachedAsCopy {
		t.Errorf("copy = %+v, want it to take %s over", r2, r1.ItemPID)
	}
	if got := stateOf(t, st, r1.ItemPID); got != model.StatePresent {
		t.Errorf("state = %s, want present", got)
	}
	if got := rolesOf(t, st, r1.ItemPID); got[r2.FilePID] != "primary" || got[r1.FilePID] != "alternate" {
		t.Errorf("edges = %v, want the copy primary and the gone original an alternate", got)
	}
	var updated bool
	for _, c := range changesAfter(t, st, seq) {
		updated = updated || (c.EntityType == "item" && c.EntityPID == r1.ItemPID)
	}
	if !updated {
		t.Error("the revived item emitted no update")
	}

	q1, q2 := filepath.Join(root, "c", "2.mp3"), filepath.Join(otherRoot, "d", "2.mp3")
	touch(t, q1)
	s1 := mustPut(t, st, input(lib.ID, q1, "sha256:OTHER", "sha256:D1", "Back"))
	if _, err := st.MarkFilesMissing(ctx, []model.PID{s1.FilePID}); err != nil {
		t.Fatal(err)
	}
	touch(t, q2)
	s2 := mustPut(t, st, input(other.ID, q2, "sha256:OTHER", "sha256:D2", "Back Copy"))
	if !s2.AttachedAsCopy || stateOf(t, st, s1.ItemPID) != model.StatePresent {
		t.Errorf("copy beside a restored primary = %+v, state %s, want an alternate and the item present",
			s2, stateOf(t, st, s1.ItemPID))
	}
}

// TestRestoredBackupRevivesItsItem: when the primary and its copy are both gone, the item
// is missing; restoring only the copy at its old path brings the item back on it.
func TestRestoredBackupRevivesItsItem(t *testing.T) {
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	p1, p2 := filepath.Join(root, "a", "1.mp3"), filepath.Join(root, "b", "1.mp3")
	r1, r2 := putCopyPair(t, st, lib, p1, p2)
	for _, p := range []string{p1, p2} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	if mr, err := st.MarkFilesMissing(ctx, []model.PID{r1.FilePID, r2.FilePID}); err != nil || mr.Marked != 1 {
		t.Fatalf("reconcile = %+v (err %v), want the item missing", mr, err)
	}
	touch(t, p2)
	in2 := input(lib.ID, p2, "sha256:SAME", "sha256:C2", "Retagged")
	in2.Track.Album = "Other Album"
	mustPut(t, st, in2)
	if got := stateOf(t, st, r1.ItemPID); got != model.StatePresent {
		t.Errorf("state = %s, want the restored backup to bring the item back", got)
	}
	if got := rolesOf(t, st, r1.ItemPID); got[r2.FilePID] != "primary" {
		t.Errorf("edges = %v, want the restored backup primary", got)
	}
}

// TestUnreachableCopyLeavesTheItemMissing: a copy under an absent library root (an
// unplugged drive) is not promoted in place of a gone primary, whether reconciliation
// finds the primary gone or the primary is trashed. The item, playable from no file, is
// marked missing, and comes back when the drive's copy is read again.
func TestUnreachableCopyLeavesTheItemMissing(t *testing.T) {
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	drive, driveRoot := addCopyLibrary(t, st)
	p1, q1 := filepath.Join(root, "1.mp3"), filepath.Join(root, "2.mp3")
	pd, qd := filepath.Join(driveRoot, "1.mp3"), filepath.Join(driveRoot, "2.mp3")
	for _, p := range []string{p1, q1, pd, qd} {
		touch(t, p)
	}
	r1 := mustPut(t, st, input(lib.ID, p1, "sha256:ONE", "sha256:C1", "One"))
	rd := mustPut(t, st, input(drive.ID, pd, "sha256:ONE", "sha256:CD", "One"))
	s1 := mustPut(t, st, input(lib.ID, q1, "sha256:TWO", "sha256:D1", "Two"))
	mustPut(t, st, input(drive.ID, qd, "sha256:TWO", "sha256:DD", "Two"))
	if err := os.RemoveAll(driveRoot); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(p1); err != nil {
		t.Fatal(err)
	}
	mr, err := st.MarkFilesMissing(ctx, []model.PID{r1.FilePID})
	if err != nil {
		t.Fatal(err)
	}
	if len(mr.Promoted) != 0 || mr.Marked != 1 || stateOf(t, st, r1.ItemPID) != model.StateMissing {
		t.Errorf("reconcile = %+v, state %s, want nothing promoted and the item missing", mr, stateOf(t, st, r1.ItemPID))
	}
	res, err := st.DetachFile(ctx, s1.FilePID)
	if err != nil {
		t.Fatal(err)
	}
	if got := stateOf(t, st, s1.ItemPID); got != model.StateMissing || len(res.Promoted) != 1 {
		t.Errorf("trash = %+v, state %s, want the unplugged copy made primary and the item missing", res, got)
	}

	touch(t, pd)
	mustPut(t, st, input(drive.ID, pd, "sha256:ONE", "sha256:CD", "One"))
	if got := stateOf(t, st, r1.ItemPID); got != model.StatePresent {
		t.Errorf("state after the drive came back = %s, want present", got)
	}
	if got := rolesOf(t, st, r1.ItemPID); got[rd.FilePID] != "primary" {
		t.Errorf("edges = %v, want the drive's copy primary", got)
	}
}

func TestMarkFilesMissingDropsAGoneCopy(t *testing.T) {
	ctx := context.Background()
	st, dbPath, _ := openStoreAt(t)
	lib, root := addCopyLibrary(t, st)
	p1, p2 := filepath.Join(root, "a", "1.mp3"), filepath.Join(root, "b", "1.mp3")
	r1, r2 := putCopyPair(t, st, lib, p1, p2)
	if err := st.PutAnalysis(ctx, model.AnalysisInput{AnalysisVersion: 1, Fingerprint: model.FingerprintInput{
		FilePID: r2.FilePID, EssenceHash: "sha256:SAME", AlgoVersion: 1, DurationBucket: 1, FP: []byte{1}, Terms: []int64{7},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p2); err != nil {
		t.Fatal(err)
	}
	res, err := st.MarkFilesMissing(ctx, []model.PID{r2.FilePID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Marked != 0 || res.Dropped != 1 || len(res.Promoted) != 0 {
		t.Fatalf("reconcile = %+v, want the copy dropped", res)
	}
	if got := rolesOf(t, st, r1.ItemPID); len(got) != 1 || got[r1.FilePID] != "primary" {
		t.Errorf("edges = %v, want the primary alone", got)
	}
	var terms int
	if err := roConn(t, dbPath).QueryRowContext(ctx, "SELECT COUNT(*) FROM fingerprint_term").Scan(&terms); err != nil || terms != 0 {
		t.Errorf("fingerprint terms left = %d (err %v), want the dropped row's gone", terms, err)
	}
}

// TestCopyRekeyedPrimaryPromotesTheAlternate: re-encoding the primary in place moves it
// to a new item, and the old item keeps its pid and play state on the promoted copy
// rather than being deleted.
func TestCopyRekeyedPrimaryPromotesTheAlternate(t *testing.T) {
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	p1, p2 := filepath.Join(root, "a", "1.mp3"), filepath.Join(root, "b", "1.mp3")
	r1, r2 := putCopyPair(t, st, lib, p1, p2)
	if err := st.MarkPlayed(ctx, "", r1.ItemPID, true, nil); err != nil {
		t.Fatal(err)
	}
	r := mustPut(t, st, input(lib.ID, p1, "sha256:REENCODED", "sha256:C1x", "Original"))
	if r.ItemPID == r1.ItemPID || !r.ItemCreated {
		t.Fatalf("re-encoded file = %+v, want a new item", r)
	}
	if got := promotedPIDs(r.Promoted); len(got) != 1 || got[0] != r2.FilePID {
		t.Errorf("promoted = %v, want the copy reported for a re-read", got)
	}
	it, err := st.ItemByPID(ctx, r1.ItemPID)
	if err != nil || it.FilePID != r2.FilePID {
		t.Fatalf("old item = %+v (err %v), want it kept on the copy", it, err)
	}
	if ps, err := st.PlayStateFor(ctx, "", r1.ItemPID); err != nil || !ps.Played {
		t.Errorf("play state = %+v (err %v), want it kept", ps, err)
	}
}

// TestCopyCountsOnceInRollupsAndStats: a copy adds no running time to its genre or the
// library total, and costs nothing toward a size budget, while db verify stays clean.
func TestCopyCountsOnceInRollupsAndStats(t *testing.T) {
	ctx := context.Background()
	st, dbPath, _ := openStoreAt(t)
	lib, root := addCopyLibrary(t, st)
	put := func(dir, content string) *model.ScanItemResult {
		p := filepath.Join(root, dir, "1.mp3")
		touch(t, p)
		in := input(lib.ID, p, "sha256:SAME", content, "Song")
		in.Track.Genre, in.File.DurationMS, in.File.Size = "Rock", 1000, 600_000
		return mustPut(t, st, in)
	}
	r1 := put("a", "sha256:C1")
	if r := put("b", "sha256:C2"); !r.AttachedAsCopy {
		t.Fatalf("second put = %+v, want a copy", r)
	}
	var genreMS int64
	if err := roConn(t, dbPath).QueryRowContext(ctx, "SELECT total_duration_ms FROM genre_rollup").Scan(&genreMS); err != nil || genreMS != 1000 {
		t.Errorf("genre running time = %d ms (err %v), want 1000", genreMS, err)
	}
	stats, err := st.Stats(ctx, "", 10)
	if err != nil || stats.TotalDuration != 1000 {
		t.Errorf("library running time = %+v (err %v), want 1000 ms", stats, err)
	}
	items, err := st.QueryItems(ctx, query.New(query.EntityItems).Limit(1).LimitBy(query.LimitMegabytes).Build(), "")
	if err != nil || len(items) != 1 || items[0].PID != r1.ItemPID {
		t.Errorf("a one-megabyte budget = %d items (err %v), want the item priced at its primary alone", len(items), err)
	}
	assertConsistent(t, st)
}

// TestCopyAnalysisSkipsASameAudioAlternate: a copy of the primary's audio is not
// analyzed (its measurements would be the primary's), while another encoding is, its
// fingerprint and loudness being its own.
func TestCopyAnalysisSkipsASameAudioAlternate(t *testing.T) {
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	p1, p2 := filepath.Join(root, "a", "1.mp3"), filepath.Join(root, "b", "1.mp3")
	r1, r2 := putCopyPair(t, st, lib, p1, p2)
	pm, pf := filepath.Join(root, "m", "x.mp3"), filepath.Join(root, "f", "x.flac")
	touch(t, pm)
	touch(t, pf)
	mp3 := input(lib.ID, pm, "sha256:EMP3", "sha256:CMP3", "X")
	mp3.Item.IdentityKey, mp3.File.Codec, mp3.File.Bitrate = "mbid:rec-x", "mp3", 320000
	flac := input(lib.ID, pf, "sha256:EFLAC", "sha256:CFLAC", "X")
	flac.Item.IdentityKey, flac.File.Codec, flac.File.SampleRate, flac.File.BitDepth = "mbid:rec-x", "flac", 44100, 16
	rm, rf := mustPut(t, st, mp3), mustPut(t, st, flac)

	n, err := st.CountFilesNeedingAnalysis(ctx, 1)
	if err != nil || n != 3 {
		t.Errorf("files needing analysis = %d (err %v), want 3 (not the copy)", n, err)
	}
	files, err := st.FilesNeedingAnalysis(ctx, 1, nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[model.PID]bool{}
	for _, f := range files {
		got[f.PID] = true
	}
	if !got[r1.FilePID] || got[r2.FilePID] || !got[rm.FilePID] || !got[rf.FilePID] {
		t.Errorf("files needing analysis = %v, want the primary and both encodings, not the copy %s", got, r2.FilePID)
	}
	if n, err := st.CountItemsMissingReplayGain(ctx); err != nil || n != 3 {
		t.Errorf("files missing ReplayGain = %d (err %v), want 3", n, err)
	}
}

// TestFingerprintCandidatesNameAnItem: a candidate always names the item it backs: an
// alternate encoding names its item, and a rip's shared file is left out.
func TestFingerprintCandidatesNameAnItem(t *testing.T) {
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	pm, pf, rip := filepath.Join(root, "m", "x.mp3"), filepath.Join(root, "f", "x.flac"), filepath.Join(root, "r", "album.flac")
	for _, p := range []string{pm, pf, rip} {
		touch(t, p)
	}
	mp3 := input(lib.ID, pm, "sha256:EMP3", "sha256:CMP3", "X")
	mp3.Item.IdentityKey, mp3.File.Codec = "mbid:rec-x", "mp3"
	flac := input(lib.ID, pf, "sha256:EFLAC", "sha256:CFLAC", "X")
	flac.Item.IdentityKey, flac.File.Codec = "mbid:rec-x", "flac"
	rm, rf := mustPut(t, st, mp3), mustPut(t, st, flac)
	rr, err := st.PutScannedVirtualTracks(ctx, vtrackInput(lib.ID, rip, "sha256:VE", "sha256:VC", 600, [][2]int64{{0, 9}, {9, 0}}))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []struct {
		pid     model.PID
		essence string
	}{{rm.FilePID, "sha256:EMP3"}, {rf.FilePID, "sha256:EFLAC"}, {rr.FilePID, "sha256:VE"}} {
		if err := st.PutAnalysis(ctx, model.AnalysisInput{AnalysisVersion: 1, Fingerprint: model.FingerprintInput{
			FilePID: f.pid, EssenceHash: f.essence, AlgoVersion: 1, DurationBucket: 1, FP: []byte{1}, Terms: []int64{1, 2, 3},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	cands, err := st.FingerprintCandidates(ctx, rf.FilePID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].FilePID != rm.FilePID || cands[0].ItemPID != rf.ItemPID {
		t.Errorf("candidates = %+v, want only the mp3 alternate, naming its item %s", cands, rf.ItemPID)
	}
}

// TestCopyMemberFilesFanOut: an album's member files take in its tracks' copies, so an
// album-level write reaches them.
func TestCopyMemberFilesFanOut(t *testing.T) {
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	p1, p2 := filepath.Join(root, "a", "1.mp3"), filepath.Join(root, "b", "1.mp3")
	r1, r2 := putCopyPair(t, st, lib, p1, p2)
	it, err := st.ItemByPID(ctx, r1.ItemPID)
	if err != nil {
		t.Fatal(err)
	}
	files, err := st.EntityMemberFiles(ctx, model.MergeAlbum, it.AlbumPID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[model.PID]bool{}
	for _, f := range files {
		got[f.FilePID] = true
	}
	if len(got) != 2 || !got[r1.FilePID] || !got[r2.FilePID] {
		t.Errorf("album member files = %v, want the primary and its copy", got)
	}
}

// TestPutAcquisitionForFileSkipsACopy: an import stamp on a file that joined an existing
// item as a copy records nothing, since the item was acquired before it, and says so.
func TestPutAcquisitionForFileSkipsACopy(t *testing.T) {
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	p1, p2 := filepath.Join(root, "a", "1.mp3"), filepath.Join(root, "b", "1.mp3")
	r1, _ := putCopyPair(t, st, lib, p1, p2)
	copyOf, err := st.PutAcquisitionForFile(ctx, []byte(p2), model.AcquisitionInput{SourceType: model.SourceManual})
	if err != nil || copyOf != r1.ItemPID {
		t.Fatalf("stamp on the copy = %q (err %v), want it reported as a copy of %s", copyOf, err, r1.ItemPID)
	}
	if _, err := st.AcquisitionByItem(ctx, r1.ItemPID); err == nil {
		t.Error("the item gained an acquisition row from its copy's import")
	}
	if copyOf, err := st.PutAcquisitionForFile(ctx, []byte(p1), model.AcquisitionInput{SourceType: model.SourceManual}); err != nil || copyOf != "" {
		t.Errorf("stamp on the primary = %q (err %v), want it recorded", copyOf, err)
	}
}

// TestCopyLibraryScopeCoversEveryFile: an item whose primary sits in one library and a
// copy in another is in scope for both: search narrowed to either finds it, the library
// field matches either, and the library facet counts it under each. Its own library
// stays the primary's, and each of its files names its own.
func TestCopyLibraryScopeCoversEveryFile(t *testing.T) {
	ctx := context.Background()
	st, libA, rootA := openCopyStore(t)
	libB, rootB := addCopyLibrary(t, st)
	libC, _ := addCopyLibrary(t, st)
	pa, pb := filepath.Join(rootA, "1.mp3"), filepath.Join(rootB, "1.mp3")
	touch(t, pa)
	touch(t, pb)
	ra := mustPut(t, st, input(libA.ID, pa, "sha256:SAME", "sha256:CA", "Wandering Star"))
	rb := mustPut(t, st, input(libB.ID, pb, "sha256:SAME", "sha256:CB", "Wandering Star"))
	if !rb.AttachedAsCopy {
		t.Fatalf("copy = %+v, want it attached", rb)
	}

	for _, tc := range []struct {
		lib  model.PID
		want int
	}{{libA.PID, 1}, {libB.PID, 1}, {libC.PID, 0}} {
		res, err := st.Search(ctx, "wandering", read.SearchOptions{Libraries: []model.PID{tc.lib}})
		if err != nil || len(res.Tracks) != tc.want {
			t.Errorf("search in %s = %+v (err %v), want %d tracks", tc.lib, res, err, tc.want)
		}
		items, err := st.QueryItems(ctx, query.New(query.EntityItems).Where("library", query.OpIs, string(tc.lib)).Build(), "")
		if err != nil || len(items) != tc.want {
			t.Errorf("library is %s = %d items (err %v), want %d", tc.lib, len(items), err, tc.want)
		}
	}
	if items, err := st.QueryItems(ctx, query.New(query.EntityItems).Where("library", query.OpIsNot, string(libB.PID)).Build(), ""); err != nil || len(items) != 0 {
		t.Errorf("library isNot B = %d items (err %v), want none: the item has a file there", len(items), err)
	}
	facet, err := st.Facet(ctx, query.New(query.EntityItems).Build(), read.GroupLibrary, "", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	counts := map[model.PID]int{}
	for _, b := range facet.Buckets {
		counts[b.EntityPID] = b.Count
	}
	if counts[libA.PID] != 1 || counts[libB.PID] != 1 {
		t.Errorf("library facet = %+v, want the item under both A and B", facet.Buckets)
	}
	it, err := st.ItemByPID(ctx, ra.ItemPID)
	if err != nil || it.LibraryPID != libA.PID {
		t.Fatalf("item library = %+v (err %v), want A", it, err)
	}
	if info, err := st.EntityByPID(ctx, read.EntityArtist, it.ArtistPID); err != nil || !slices.Equal(info.LibraryPIDs, []model.PID{libA.PID, libB.PID}) {
		t.Errorf("artist libraries = %+v (err %v), want A and B", info, err)
	}
	refs, err := st.ItemFiles(ctx, ra.ItemPID)
	if err != nil {
		t.Fatal(err)
	}
	libs := map[model.PID]model.PID{}
	for _, r := range refs {
		libs[r.FilePID] = r.LibraryPID
	}
	if libs[ra.FilePID] != libA.PID || libs[rb.FilePID] != libB.PID {
		t.Errorf("file libraries = %v, want each file's own", libs)
	}
}

// TestAuditFilesNameACopysItem: the file-level audit pins a copy's findings on the item
// it backs, as it does the primary's.
func TestAuditFilesNameACopysItem(t *testing.T) {
	ctx := context.Background()
	st, lib, root := openCopyStore(t)
	p1, p2 := filepath.Join(root, "a", "1.mp3"), filepath.Join(root, "b", "1.mp3")
	r1, r2 := putCopyPair(t, st, lib, p1, p2)
	files, err := st.AuditFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owners := map[model.PID]model.PID{}
	for _, f := range files {
		owners[f.PID] = f.ItemPID
	}
	if len(files) != 2 || owners[r1.FilePID] != r1.ItemPID || owners[r2.FilePID] != r1.ItemPID {
		t.Errorf("audit files = %+v, want both naming %s", files, r1.ItemPID)
	}
}

// TestLibraryFieldOperators: the library field over every file edge: in matches an item
// with a file in any listed library, notIn is a deny-list that keeps a fileless item, and
// the presence pair splits items with files from those without.
func TestLibraryFieldOperators(t *testing.T) {
	ctx := context.Background()
	st, libA, rootA := openCopyStore(t)
	libB, rootB := addCopyLibrary(t, st)
	pa, pb, pg := filepath.Join(rootA, "1.mp3"), filepath.Join(rootB, "1.mp3"), filepath.Join(rootA, "gone.mp3")
	for _, p := range []string{pa, pb, pg} {
		touch(t, p)
	}
	ra := mustPut(t, st, input(libA.ID, pa, "sha256:SAME", "sha256:CA", "Both"))
	mustPut(t, st, input(libB.ID, pb, "sha256:SAME", "sha256:CB", "Both"))
	rg := mustPut(t, st, input(libA.ID, pg, "sha256:GONE", "sha256:CG", "Fileless"))
	if _, err := st.DetachFile(ctx, rg.FilePID); err != nil {
		t.Fatal(err)
	}
	match := func(q query.Query) []model.PID {
		t.Helper()
		items, err := st.QueryItems(ctx, q, "")
		if err != nil {
			t.Fatal(err)
		}
		var out []model.PID
		for _, it := range items {
			out = append(out, it.PID)
		}
		return out
	}
	both, fileless := []model.PID{ra.ItemPID}, []model.PID{rg.ItemPID}
	for _, tc := range []struct {
		name string
		q    query.Query
		want []model.PID
	}{
		{"in B", query.New(query.EntityItems).WhereValues("library", query.OpIn, string(libB.PID), "nope").Build(), both},
		{"notIn B", query.New(query.EntityItems).WhereValues("library", query.OpNotIn, string(libB.PID)).Build(), fileless},
		{"notIn A and B", query.New(query.EntityItems).WhereValues("library", query.OpNotIn, string(libA.PID), string(libB.PID)).Build(), fileless},
		{"isPresent", query.New(query.EntityItems).Where("library", query.OpIsPresent, nil).Build(), both},
		{"isMissing", query.New(query.EntityItems).Where("library", query.OpIsMissing, nil).Build(), fileless},
	} {
		if got := match(tc.q); !slices.Equal(got, tc.want) {
			t.Errorf("%s = %v, want %v", tc.name, got, tc.want)
		}
	}
}
