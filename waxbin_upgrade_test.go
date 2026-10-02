package waxbin_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
)

// TestFindUpgradesGroupsAltEncodings verifies the quality/upgrade policy groups
// two encodings of one recording and leaves an unrelated track ungrouped, marking
// exactly one keeper per group.
func TestFindUpgradesGroupsAltEncodings(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")

	const rate = 22050
	orig := testaudio.RichSignal(rate, 20, testaudio.MusicalPartials, 1)
	transcoded := testaudio.Reencode(orig, 0.85, 42) // same recording, different bytes
	other := testaudio.RichSignal(rate, 20, testaudio.AltPartials, 7)

	writeFile(t, filepath.Join(root, "alpha.wav"), testaudio.EncodeWAV16(rate, orig))
	writeFile(t, filepath.Join(root, "beta.wav"), testaudio.EncodeWAV16(rate, transcoded))
	writeFile(t, filepath.Join(root, "gamma.wav"), testaudio.EncodeWAV16(rate, other))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if _, err := lib.Analyze(ctx, waxbin.AnalyzeOptions{}); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	groups, err := lib.FindUpgrades(ctx)
	if err != nil {
		t.Fatalf("find upgrades: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("want 1 alt-encoding group (alpha+beta), got %d: %+v", len(groups), groups)
	}
	g := groups[0]
	if len(g.Members) != 2 {
		t.Fatalf("group should have 2 members, got %d", len(g.Members))
	}

	bestCount := 0
	inGroup := map[model.PID]bool{}
	for _, m := range g.Members {
		inGroup[m.ItemPID] = true
		if m.Best {
			bestCount++
		}
		if m.Codec == "" {
			t.Errorf("member %s has no codec quality read", m.ItemPID)
		}
	}
	if bestCount != 1 {
		t.Errorf("exactly one member should be the keeper, got %d", bestCount)
	}
	alpha := itemPIDByTitle(t, ctx, lib, "alpha")
	beta := itemPIDByTitle(t, ctx, lib, "beta")
	gamma := itemPIDByTitle(t, ctx, lib, "gamma")
	if !inGroup[alpha] || !inGroup[beta] {
		t.Errorf("group should contain alpha and beta, got %+v", g.Members)
	}
	if inGroup[gamma] {
		t.Errorf("gamma (unrelated) must not be grouped")
	}
}

// TestFindUpgradesSkipsWhatItCannotResolve: an analyzed catalog holding a byte-identical
// pair, a cue rip, and a fingerprinted file no item owns (a row an older catalog left)
// still lists its groups: a candidate that names no item is passed over rather than
// aborting the listing.
func TestFindUpgradesSkipsWhatItCannotResolve(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	const rate = 22050
	orig := testaudio.RichSignal(rate, 20, testaudio.MusicalPartials, 1)
	writeFile(t, filepath.Join(root, "alpha.wav"), testaudio.EncodeWAV16(rate, orig))
	writeFile(t, filepath.Join(root, "copy", "alpha.wav"), testaudio.EncodeWAV16(rate, orig))
	writeFile(t, filepath.Join(root, "beta.wav"), testaudio.EncodeWAV16(rate, testaudio.Reencode(orig, 0.85, 42)))
	rip := testaudio.EncodeWAV16(rate, testaudio.RichSignal(rate, 20, testaudio.AltPartials, 7))
	writeFile(t, filepath.Join(root, "rip", "album.wav"), rip)
	writeFile(t, filepath.Join(root, "rip", "album.cue"), []byte("FILE \"album.wav\" WAVE\n"+
		"  TRACK 01 AUDIO\n    TITLE \"One\"\n    INDEX 01 00:00:00\n"+
		"  TRACK 02 AUDIO\n    TITLE \"Two\"\n    INDEX 01 00:10:00\n"))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if _, err := lib.Analyze(ctx, waxbin.AnalyzeOptions{}); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	// The ownerless row: beta's file and fingerprint cloned under a path no item holds.
	rawExec(t, db, `INSERT INTO file (pid, library_id, path, display_path, rel_path, kind, size, mtime_ns,
			content_hash, essence_hash, scan_state, first_seen, last_seen)
		SELECT 'ORPHANFILE0000000000000000', library_id, CAST('/nowhere/beta.wav' AS BLOB), '/nowhere/beta.wav',
			CAST('beta.wav' AS BLOB), kind, size, mtime_ns, content_hash, essence_hash, scan_state, first_seen, last_seen
		FROM file WHERE display_path LIKE '%/beta.wav'`)
	rawExec(t, db, `INSERT INTO fingerprint (file_id, essence_hash, algo_version, duration_bucket, fp)
		SELECT o.id, fp.essence_hash, fp.algo_version, fp.duration_bucket, fp.fp FROM fingerprint fp
		JOIN file b ON b.id = fp.file_id AND b.display_path LIKE '%/beta.wav' AND b.pid <> 'ORPHANFILE0000000000000000'
		JOIN file o ON o.pid = 'ORPHANFILE0000000000000000'`)
	rawExec(t, db, `INSERT INTO fingerprint_term (term, file_id)
		SELECT ft.term, o.id FROM fingerprint_term ft
		JOIN file b ON b.id = ft.file_id AND b.display_path LIKE '%/beta.wav' AND b.pid <> 'ORPHANFILE0000000000000000'
		JOIN file o ON o.pid = 'ORPHANFILE0000000000000000'`)

	groups, err := lib.FindUpgrades(ctx)
	if err != nil {
		t.Fatalf("find upgrades: %v", err)
	}
	alpha, beta := itemPIDByTitle(t, ctx, lib, "alpha"), itemPIDByTitle(t, ctx, lib, "beta")
	var found bool
	for _, g := range groups {
		in := map[model.PID]bool{}
		for _, m := range g.Members {
			if m.ItemPID == "" {
				t.Errorf("group %+v has a member with no item", g)
			}
			in[m.ItemPID] = true
		}
		found = found || (in[alpha] && in[beta])
	}
	if !found {
		t.Errorf("groups = %+v, want alpha and beta grouped", groups)
	}
}

// TestFindUpgradesListsAnItemsLesserEncoding: an item holding an MP3 alternate under a
// FLAC primary (one recording id) is a group of its own, primary first and best, the
// alternate named by its file so a host can delete it with PlanDeleteFiles.
func TestFindUpgradesListsAnItemsLesserEncoding(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	signal := testaudio.ReferenceSignal(44100, 3*time.Second)
	pf, pm := filepath.Join(root, "flac", "song.flac"), filepath.Join(root, "mp3", "song.mp3")
	writeFile(t, pf, testaudio.EncodeAs(t, "flac", "", 44100, signal))
	writeFile(t, pm, testaudio.EncodeAs(t, "mp3", "", 44100, signal))
	for _, p := range []string{pf, pm} {
		if _, err := meta.NewWriter().Apply(ctx, p, []meta.TagEdit{
			{Key: "TITLE", Values: []string{"Song"}},
			{Key: "MUSICBRAINZ_TRACKID", Values: []string{"8f1c9b2a-1111-4222-8333-944455556666"}},
		}); err != nil {
			t.Fatalf("tag %s: %v", p, err)
		}
	}
	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	item := itemPIDByTitle(t, ctx, lib, "Song")
	var flacPID, mp3PID model.PID
	refs, err := lib.ItemFiles(ctx, item)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range refs {
		switch string(r.Path) {
		case pf:
			flacPID = r.FilePID
		case pm:
			mp3PID = r.FilePID
		}
	}
	if len(refs) != 2 || flacPID == "" || mp3PID == "" {
		t.Fatalf("files = %+v, want the flac and the mp3 on one item", refs)
	}

	groups, err := lib.FindUpgrades(ctx)
	if err != nil {
		t.Fatalf("find upgrades: %v", err)
	}
	if len(groups) != 1 || len(groups[0].Members) != 2 {
		t.Fatalf("groups = %+v, want the item's two encodings", groups)
	}
	best, lesser := groups[0].Members[0], groups[0].Members[1]
	if best.ItemPID != item || best.FilePID != flacPID || !best.Best || !best.Lossless ||
		lesser.ItemPID != item || lesser.FilePID != mp3PID || lesser.Best {
		t.Errorf("members = %+v, want the flac best then the mp3, both on %s", groups[0].Members, item)
	}
}
