package sqlite_test

import (
	"context"
	"strconv"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// TestSetLibraryReadOnly: the flag reads back through every library read and survives
// the root being ensured again, a real change emits one library delta and a repeat
// none, and the podcast library cannot be flagged.
func TestSetLibraryReadOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _, lib := openStoreAt(t)
	seq0, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		got, err := st.SetLibraryReadOnly(ctx, lib.PID, true)
		if err != nil || got.PID != lib.PID || got.Root == nil || !got.ReadOnly {
			t.Fatalf("flag #%d = %+v (err %v), want the library row with the flag set", i+1, got, err)
		}
	}
	rows, err := st.ChangesSince(ctx, seq0)
	if err != nil || len(rows) != 1 || rows[0].EntityType != "library" || rows[0].EntityPID != lib.PID || rows[0].Op != model.OpUpdate {
		t.Fatalf("deltas = %+v (err %v), want one library update", rows, err)
	}
	libs, err := st.Libraries(ctx)
	if err != nil || len(libs) != 1 || !libs[0].ReadOnly {
		t.Fatalf("libraries = %+v (err %v), want the flag read back", libs, err)
	}
	again, err := st.EnsureLibrary(ctx, &model.Library{Root: lib.Root, DisplayRoot: lib.DisplayRoot, Mode: lib.Mode, Profile: lib.Profile})
	if err != nil || !again.ReadOnly {
		t.Fatalf("re-ensured library = %+v (err %v), want the flag kept", again, err)
	}
	if ro, err := st.LibraryReadOnly(ctx, lib.ID); err != nil || !ro {
		t.Fatalf("LibraryReadOnly = %v (err %v), want true", ro, err)
	}
	if got, err := st.SetLibraryReadOnly(ctx, lib.PID, false); err != nil || got.ReadOnly {
		t.Fatalf("clearing the flag = %+v (err %v), want it cleared", got, err)
	}
	if ro, err := st.LibraryReadOnly(ctx, lib.ID); err != nil || ro {
		t.Fatalf("LibraryReadOnly after clearing = %v (err %v), want false", ro, err)
	}

	podID, err := st.EnsurePodcastLibrary(ctx, "/pods")
	if err != nil {
		t.Fatal(err)
	}
	libs, _ = st.Libraries(ctx)
	for _, l := range libs {
		if l.ID == podID {
			if _, err := st.SetLibraryReadOnly(ctx, l.PID, true); !waxerr.Is(err, waxerr.CodeInvalid) {
				t.Errorf("flagging the podcast library = %v, want CodeInvalid", err)
			}
		}
	}
	if _, err := st.SetLibraryReadOnly(ctx, "01NOSUCHLIBRARY000000000000", true); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("flagging an unknown library = %v, want CodeNotFound", err)
	}
	if _, err := st.LibraryReadOnly(ctx, 999999); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("reading an unknown library = %v, want CodeNotFound", err)
	}
}

// TestReadOnlyLibraryIsLeftOutOfTheWritebacks: a file in a read-only library is not
// handed to the enrichment or ReplayGain write-backs, and its values stay owed, so the
// first pass after the flag clears writes them. The rows name the library for the
// write loops' own live check.
func TestReadOnlyLibraryIsLeftOutOfTheWritebacks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)
	pid := seedEnrichTrack(t, st, lib.ID)
	itemID := itemRowID(t, db, pid)
	filePID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM file"))
	if err := st.ApplyItemFields(ctx, model.ItemFieldsEnrichment{
		ItemID: itemID, PID: pid, Matched: true, Provider: "p", Fields: map[string]string{"composer": "Roger Waters"},
	}); err != nil {
		t.Fatalf("fill: %v", err)
	}
	if err := st.ApplyAlbumFields(ctx, model.AlbumFieldsEnrichment{
		AlbumID: int64(scalarQueryInt(t, db, "SELECT id FROM album")), PID: model.PID(scalarQueryStr(t, db, "SELECT pid FROM album")),
		Matched: true, Provider: "p", Fields: map[string]string{"label": "Harvest"},
	}); err != nil {
		t.Fatalf("label: %v", err)
	}
	if err := st.PutAnalysis(ctx, model.AnalysisInput{
		AnalysisVersion:  1,
		Fingerprint:      model.FingerprintInput{FilePID: filePID, EssenceHash: "ess-a", AlgoVersion: 1, FP: []byte{}},
		Loudness:         &model.LoudnessData{IntegratedLUFS: -14, TrackGainDB: -4, TrackPeak: 0.9},
		MeasureCompleted: true,
	}); err != nil {
		t.Fatalf("analysis: %v", err)
	}
	owed := func(step string, want int) {
		t.Helper()
		tags, err := st.EnrichmentWriteback(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		labels, err := st.EnrichedAlbumLabelFiles(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		gains, err := st.ReplayGainWriteback(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(tags) != want || len(labels) != want || len(gains) != want {
			t.Fatalf("%s: owed %d tags, %d labels, %d gains, want %d of each", step, len(tags), len(labels), len(gains), want)
		}
		if want == 1 && (tags[0].LibraryID != lib.ID || labels[0].LibraryID != lib.ID || gains[0].LibraryID != lib.ID) {
			t.Fatalf("%s: rows name libraries %d/%d/%d, want %d", step, tags[0].LibraryID, labels[0].LibraryID, gains[0].LibraryID, lib.ID)
		}
	}
	owed("writable", 1)
	if _, err := st.SetLibraryReadOnly(ctx, lib.PID, true); err != nil {
		t.Fatal(err)
	}
	owed("read-only", 0)
	if _, err := st.SetLibraryReadOnly(ctx, lib.PID, false); err != nil {
		t.Fatal(err)
	}
	owed("writable again", 1)
}

// TestTrashEntryNamesItsLibrary: a trash entry carries the library its file was in, so
// the facade can honor that library's flag without matching paths.
func TestTrashEntryNamesItsLibrary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	seedEnrichTrack(t, st, lib.ID)
	filePID := model.PID(scalarQueryStr(t, roConn(t, dbPath), "SELECT pid FROM file"))
	tpidres, err := st.TrashFile(ctx, model.TrashFileInput{FilePID: filePID, TrashPath: []byte("/trash/a.mp3"), TrashDisplay: "/trash/a.mp3"})
	if err != nil {
		t.Fatalf("TrashFile: %v", err)
	}
	tpid := tpidres.TrashPID
	e, err := st.ActiveTrashByPID(ctx, tpid)
	if err != nil || e.LibraryPID != lib.PID {
		t.Fatalf("entry = %+v (err %v), want library %s", e, err, lib.PID)
	}
	list, err := st.TrashEntries(ctx, false, 0, 0)
	if err != nil || len(list) != 1 || list[0].LibraryPID != lib.PID {
		t.Fatalf("entries = %+v (err %v), want library %s", list, err, lib.PID)
	}
}

// TestAWideReachStaysItsOwn: a reach naming more ids than one statement can bind still
// selects only its own files for both write-backs, rather than everything owed.
func TestAWideReachStaysItsOwn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)
	var ids []int64
	for i, name := range []string{"a", "b"} {
		in := seedEnrichTrackInput(lib.ID)
		in.File.Path, in.File.DisplayPath, in.File.RelPath = []byte("/lib/"+name+".mp3"), "/lib/"+name+".mp3", []byte(name+".mp3")
		in.File.ContentHash, in.File.EssenceHash = "c-"+name, "ess-"+name
		in.Item.IdentityKey = "essence:ess-" + name
		in.Track.Album = "Album " + name
		res, err := st.PutScannedTrack(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		id := itemRowID(t, db, res.ItemPID)
		ids = append(ids, id)
		if err := st.ApplyItemFields(ctx, model.ItemFieldsEnrichment{
			ItemID: id, PID: res.ItemPID, Matched: true, Provider: "p", Fields: map[string]string{"composer": "C"},
		}); err != nil {
			t.Fatal(err)
		}
		albumID := int64(scalarQueryInt(t, db, "SELECT album_id FROM track WHERE item_id = ?", id))
		if err := st.ApplyAlbumFields(ctx, model.AlbumFieldsEnrichment{
			AlbumID: albumID, PID: model.PID(scalarQueryStr(t, db, "SELECT pid FROM album WHERE id = ?", albumID)),
			Matched: true, Provider: "p", Fields: map[string]string{"label": "L" + strconv.Itoa(i)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	wide := make([]int64, 40000)
	for i := range wide {
		wide[i] = int64(1_000_000 + i)
	}
	wide[len(wide)-1] = ids[0]
	reach := &model.EnrichScope{FieldsItemIDs: wide}
	tags, err := st.EnrichmentWriteback(ctx, reach)
	if err != nil || len(tags) != 1 || string(tags[0].Path) != "/lib/a.mp3" {
		t.Fatalf("tag rows = %+v (err %v), want a.mp3 alone", tags, err)
	}
	labels, err := st.EnrichedAlbumLabelFiles(ctx, reach)
	if err != nil || len(labels) != 1 || string(labels[0].Path) != "/lib/a.mp3" {
		t.Fatalf("label rows = %+v (err %v), want a.mp3 alone", labels, err)
	}
}
