package audit

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/colespringer/waxbin/model"
)

// fakeStore is a hand-rolled audit.Store for exercising the auditor logic without
// a real database.
type fakeStore struct {
	dupArtists   []model.DuplicateSet
	dupGenres    []model.DuplicateSet
	dupAlbums    []model.DuplicateSet
	dupRGs       []model.DuplicateSet
	splits       []model.SplitAlbum
	inconsist    []model.AlbumIssue
	missingArt   []model.ItemRef
	missingTot   int
	missingMBID  []model.ItemRef
	missingMBTot int
	missingRG    int
	files        []model.AuditFileInfo
	pods         []*model.Podcast
	libs         []*model.Library
	drift        model.DerivedDrift
	diags        []model.FileDiagnostic
	diagStale    int
	diagTotal    int
	mismatches   []model.FileDurationMismatch
	mismatchTot  int
	copies       []model.ItemCopies
}

func (f *fakeStore) ItemsWithCopies(context.Context) ([]model.ItemCopies, error) {
	return f.copies, nil
}

func (f *fakeStore) DuplicateArtists(context.Context) ([]model.DuplicateSet, error) {
	return f.dupArtists, nil
}
func (f *fakeStore) DuplicateGenres(context.Context) ([]model.DuplicateSet, error) {
	return f.dupGenres, nil
}
func (f *fakeStore) DuplicateAlbums(context.Context) ([]model.DuplicateSet, error) {
	return f.dupAlbums, nil
}
func (f *fakeStore) DuplicateReleaseGroups(context.Context) ([]model.DuplicateSet, error) {
	return f.dupRGs, nil
}
func (f *fakeStore) SplitAlbums(context.Context) ([]model.SplitAlbum, error) { return f.splits, nil }
func (f *fakeStore) InconsistentAlbums(context.Context) ([]model.AlbumIssue, error) {
	return f.inconsist, nil
}
func (f *fakeStore) ItemsMissingArt(_ context.Context, limit int) ([]model.ItemRef, int, error) {
	if len(f.missingArt) > limit {
		return f.missingArt[:limit], f.missingTot, nil
	}
	return f.missingArt, f.missingTot, nil
}
func (f *fakeStore) ItemsMissingMBID(_ context.Context, limit int) ([]model.ItemRef, int, error) {
	if len(f.missingMBID) > limit {
		return f.missingMBID[:limit], f.missingMBTot, nil
	}
	return f.missingMBID, f.missingMBTot, nil
}
func (f *fakeStore) CountItemsMissingReplayGain(context.Context) (int, error) {
	return f.missingRG, nil
}
func (f *fakeStore) AuditFiles(context.Context) ([]model.AuditFileInfo, error) { return f.files, nil }
func (f *fakeStore) Podcasts(context.Context) ([]*model.Podcast, error)        { return f.pods, nil }
func (f *fakeStore) Libraries(context.Context) ([]*model.Library, error)       { return f.libs, nil }
func (f *fakeStore) DerivedDrift(context.Context) (model.DerivedDrift, error)  { return f.drift, nil }
func (f *fakeStore) FileDiagnostics(context.Context, model.DiagnosticFilter) ([]model.FileDiagnostic, error) {
	return f.diags, nil
}
func (f *fakeStore) DiagnosticCoverage(context.Context) (int, int, error) {
	return f.diagStale, f.diagTotal, nil
}
func (f *fakeStore) FilesDurationMismatch(_ context.Context, limit, _ int) ([]model.FileDurationMismatch, int, error) {
	if len(f.mismatches) > limit {
		return f.mismatches[:limit], f.mismatchTot, nil
	}
	return f.mismatches, f.mismatchTot, nil
}

func findingsFor(rep *Report, check model.AuditCheck) []model.AuditFinding {
	var out []model.AuditFinding
	for _, f := range rep.Findings {
		if f.Check == check {
			out = append(out, f)
		}
	}
	return out
}

func TestAuditDuplicateAndDedup(t *testing.T) {
	// The same pair reported by both MBID and collation-key must yield one finding.
	pair := []model.DuplicateMember{
		{PID: "a1", Name: "Beatles", TrackCount: 5},
		{PID: "a2", Name: "The Beatles", TrackCount: 2},
	}
	st := &fakeStore{dupArtists: []model.DuplicateSet{
		{EntityType: model.MergeArtist, Reason: "shared MBID", Members: pair},
		{EntityType: model.MergeArtist, Reason: "same collation key", Members: pair},
	}}
	rep, err := New(st, nil, nil, nil).Run(context.Background(), Config{Only: []model.AuditCheck{model.CheckDuplicateArtist}})
	if err != nil {
		t.Fatal(err)
	}
	fs := findingsFor(rep, model.CheckDuplicateArtist)
	if len(fs) != 1 {
		t.Fatalf("want 1 deduped duplicate finding, got %d", len(fs))
	}
	if fs[0].MergeType != model.MergeArtist || len(fs[0].Entities) != 2 || fs[0].Entities[0] != "a1" {
		t.Errorf("finding = %+v (survivor should be a1, the higher track count)", fs[0])
	}
}

// TestAuditAlbumNameDuplicateSaysToOrganize: a pair of albums found by name is split by
// folder, which a merge alone does not outlast, so the finding says how to keep one; and
// a pair of one name can be two releases, so the advice is conditional.
func TestAuditAlbumNameDuplicateSaysToOrganize(t *testing.T) {
	st := &fakeStore{dupAlbums: []model.DuplicateSet{{
		EntityType: model.MergeAlbum, Reason: model.ReasonSameAlbumName,
		Members: []model.DuplicateMember{{PID: "al1", Name: "Hits", TrackCount: 2}, {PID: "al2", Name: "Hits", TrackCount: 1}},
	}}}
	rep, err := New(st, nil, nil, nil).Run(context.Background(), Config{Only: []model.AuditCheck{model.CheckDuplicateAlbum}})
	if err != nil {
		t.Fatal(err)
	}
	fs := findingsFor(rep, model.CheckDuplicateAlbum)
	want := "if they are one release, tag their files alike (album, album artist, any MusicBrainz ids) and keep them in one folder, " +
		"which a scan then reads as one album; `waxbin merge` alone joins them only until a scan or edit re-resolves them"
	if len(fs) != 1 || !strings.Contains(fs[0].Message, want) {
		t.Fatalf("findings = %+v, want one saying %q", fs, want)
	}
}

// TestAuditSplitAlbumSaysToMoveTheFiles: a split album is most often split by folder, so
// its advice matches the name check's.
func TestAuditSplitAlbumSaysToMoveTheFiles(t *testing.T) {
	st := &fakeStore{splits: []model.SplitAlbum{{Artist: "A", Title: "Hits",
		Albums: []model.DuplicateMember{{PID: "al1", Name: "Hits", TrackCount: 2}, {PID: "al2", Name: "Hits", TrackCount: 1}}}}}
	rep, err := New(st, nil, nil, nil).Run(context.Background(), Config{Only: []model.AuditCheck{model.CheckSplitAlbum}})
	if err != nil {
		t.Fatal(err)
	}
	fs := findingsFor(rep, model.CheckSplitAlbum)
	want := "if they are one release, tag their files alike (album, album artist, any MusicBrainz ids) and keep them in one folder, " +
		"which a scan then reads as one album; `waxbin merge album` alone joins them only until a scan or edit re-resolves them"
	if len(fs) != 1 || !strings.Contains(fs[0].Message, want) {
		t.Fatalf("findings = %+v, want one saying %q", fs, want)
	}
}

// TestAuditRepeatedTrackNumbersSayHowToSplit: repeated track numbers come from two releases
// sharing an album, one release held twice, or untagged discs, and the fix differs for
// each, so the finding names all three; an album with only a stray year says nothing of
// the kind.
func TestAuditRepeatedTrackNumbersSayHowToSplit(t *testing.T) {
	st := &fakeStore{inconsist: []model.AlbumIssue{
		{AlbumPID: "al1", Title: "Weezer", Problem: "2 distinct years, 10 repeated track numbers", RepeatedPositions: 10},
		{AlbumPID: "al2", Title: "Malibu", Problem: "2 distinct years"},
	}}
	rep, err := New(st, nil, nil, nil).Run(context.Background(), Config{Only: []model.AuditCheck{model.CheckInconsistentMeta}})
	if err != nil {
		t.Fatal(err)
	}
	fs := findingsFor(rep, model.CheckInconsistentMeta)
	if len(fs) != 2 {
		t.Fatalf("findings = %+v, want two", fs)
	}
	want := "; repeated track numbers mean two releases share the album (give them distinct album titles or " +
		"MusicBrainz release ids), one release is here twice (keep one copy), or its discs carry no disc numbers (tag them)"
	for _, f := range fs {
		advised := strings.Contains(f.Message, want)
		if wantAdvice := f.Entities[0] == "al1"; advised != wantAdvice {
			t.Errorf("finding %q carries the causes = %t, want %t", f.Message, advised, wantAdvice)
		}
	}
}

// TestAuditSplitAlbumDefersToADuplicateFinding: a split the duplicate check already
// reports names the same merge, so with both checks run it is reported once, as the
// duplicate; a split no duplicate covers still reports, and so does the split check run on
// its own.
func TestAuditSplitAlbumDefersToADuplicateFinding(t *testing.T) {
	member := func(pid model.PID, n int) model.DuplicateMember {
		return model.DuplicateMember{PID: pid, Name: "Hits", TrackCount: n}
	}
	st := &fakeStore{
		dupAlbums: []model.DuplicateSet{{EntityType: model.MergeAlbum, Reason: model.ReasonSameAlbumName,
			Members: []model.DuplicateMember{member("al1", 2), member("al2", 1)}}},
		splits: []model.SplitAlbum{
			{Artist: "A", Title: "Hits", Albums: []model.DuplicateMember{member("al1", 2), member("al2", 1)}},
			{Artist: "B", Title: "Live", Albums: []model.DuplicateMember{member("al3", 2), member("al4", 1)}},
		},
	}
	both, err := New(st, nil, nil, nil).Run(context.Background(), Config{Only: []model.AuditCheck{model.CheckDuplicateAlbum, model.CheckSplitAlbum}})
	if err != nil {
		t.Fatal(err)
	}
	if dups, splits := findingsFor(both, model.CheckDuplicateAlbum), findingsFor(both, model.CheckSplitAlbum); len(dups) != 1 || len(splits) != 1 || splits[0].Entities[0] != "al3" {
		t.Errorf("findings = %+v, want the duplicate for Hits and the split for Live only", both.Findings)
	}
	alone, err := New(st, nil, nil, nil).Run(context.Background(), Config{Only: []model.AuditCheck{model.CheckSplitAlbum}})
	if err != nil {
		t.Fatal(err)
	}
	if splits := findingsFor(alone, model.CheckSplitAlbum); len(splits) != 2 {
		t.Errorf("split check alone = %+v, want both splits", alone.Findings)
	}
}

func TestAuditDerivedDriftIsError(t *testing.T) {
	st := &fakeStore{drift: model.DerivedDrift{ArtistRollupDrift: 3}}
	rep, err := New(st, nil, nil, nil).Run(context.Background(), Config{Only: []model.AuditCheck{model.CheckDerivedData}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Errors() != 1 {
		t.Fatalf("drift should be one error finding, got errors=%d findings=%+v", rep.Errors(), rep.Findings)
	}
}

// TestAuditAlbumYearDriftIsError: an album year that is not its members' most common
// year is derived-data drift like a stale rollup, named in the finding.
func TestAuditAlbumYearDriftIsError(t *testing.T) {
	st := &fakeStore{drift: model.DerivedDrift{AlbumYearDrift: 2}}
	rep, err := New(st, nil, nil, nil).Run(context.Background(), Config{Only: []model.AuditCheck{model.CheckDerivedData}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Errors() != 1 || !strings.Contains(rep.Findings[0].Message, "2 album-year") {
		t.Fatalf("findings = %+v, want one error naming 2 album-year drift", rep.Findings)
	}
}

func TestAuditFileChecks(t *testing.T) {
	st := &fakeStore{files: []model.AuditFileInfo{
		{PID: "f1", Path: []byte("/lib/al/song.flac"), DisplayPath: "/lib/al/song.flac", Kind: model.FileAudio},
		{PID: "f4", Path: []byte("/lib/al/what?.flac"), DisplayPath: "/lib/al/what?.flac", Kind: model.FileAudio}, // bad name
		{PID: "f5", Path: []byte("/lib/x/Track.flac"), DisplayPath: "/lib/x/Track.flac", Kind: model.FileAudio},
		{PID: "f6", Path: []byte("/lib/x/track.flac"), DisplayPath: "/lib/x/track.flac", Kind: model.FileAudio}, // case conflict with f5
	}}
	rep, err := New(st, nil, nil, nil).Run(context.Background(), Config{Only: []model.AuditCheck{
		model.CheckBadFilename, model.CheckPathConflict,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := findingsFor(rep, model.CheckBadFilename); len(got) != 1 || got[0].Path != "/lib/al/what?.flac" || got[0].FilePID != "f4" {
		t.Errorf("bad filename findings = %+v, want f4", got)
	}
	pc := findingsFor(rep, model.CheckPathConflict)
	if len(pc) != 1 || pc[0].Severity != model.SeverityError {
		t.Errorf("path conflict findings = %+v", pc)
	}
}

func TestAuditFeedValidationRSSOnly(t *testing.T) {
	// Non-RSS shows carry provider-specific feed_urls (not HTTP), so only rss feeds
	// get the invalid-URL check. All have episodes, isolating the URL finding.
	st := &fakeStore{pods: []*model.Podcast{
		{PID: "p1", Title: "YT Show", SourceType: model.SourceYouTube, FeedURL: "youtube:channel:UC123", EpisodeCount: 5},
		{PID: "p2", Title: "Bad RSS", SourceType: model.SourceRSS, FeedURL: "not a url", EpisodeCount: 5},
		{PID: "p3", Title: "Good RSS", SourceType: model.SourceRSS, FeedURL: "https://example.com/feed.xml", EpisodeCount: 5},
		{PID: "p4", Title: "Manual", SourceType: model.SourceManual, FeedURL: "manual:01ABC", EpisodeCount: 5},
	}}
	rep, err := New(st, nil, nil, nil).Run(context.Background(), Config{Only: []model.AuditCheck{model.CheckInvalidFeed}})
	if err != nil {
		t.Fatal(err)
	}
	var badURL []model.PID
	for _, f := range findingsFor(rep, model.CheckInvalidFeed) {
		if strings.Contains(f.Message, "invalid feed URL") {
			badURL = append(badURL, f.Entities[0])
		}
	}
	if len(badURL) != 1 || badURL[0] != "p2" {
		t.Errorf("invalid-feed-URL findings = %v, want only the bad RSS feed (p2)", badURL)
	}
}

func TestAuditIntegrity(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.flac")
	if err := os.WriteFile(good, []byte("real bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	goodHash, err := identity.ContentHash(good)
	if err != nil {
		t.Fatal(err)
	}
	bitrot := filepath.Join(dir, "bitrot.flac")
	if err := os.WriteFile(bitrot, []byte("changed on disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := &fakeStore{files: []model.AuditFileInfo{
		{PID: "f1", Path: []byte(good), DisplayPath: good, Kind: model.FileAudio, ContentHash: goodHash},
		{PID: "f2", Path: []byte(bitrot), DisplayPath: bitrot, Kind: model.FileAudio, ContentHash: "sha256:stale"},
		{PID: "f3", Path: []byte(filepath.Join(dir, "gone.flac")), DisplayPath: "gone.flac", Kind: model.FileAudio, ContentHash: "x"},
	}}
	probeFail := func(_ context.Context, p string) ([]string, error) {
		if filepath.Base(p) == "good.flac" {
			return nil, nil
		}
		return nil, os.ErrInvalid
	}
	rep, err := New(st, identity.ContentHash, probeFail, nil).Run(context.Background(), Config{
		Only:      []model.AuditCheck{model.CheckIntegrity, model.CheckCorruptAudio},
		Integrity: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]model.PID{bitrot: "f2", "gone.flac": "f3"}
	// bitrot (hash mismatch) + gone (missing) = 2 integrity errors.
	got := findingsFor(rep, model.CheckIntegrity)
	if len(got) != 2 {
		t.Errorf("integrity findings = %+v, want 2", got)
	}
	if rep.FilesChecked != 3 {
		t.Errorf("FilesChecked = %d, want 3", rep.FilesChecked)
	}
	// good.flac probes clean; the other two (a missing file and bitrot) fail the probe.
	corrupt := findingsFor(rep, model.CheckCorruptAudio)
	if len(corrupt) != 2 {
		t.Errorf("corrupt findings = %+v, want 2", corrupt)
	}
	for _, f := range append(got, corrupt...) {
		if f.FilePID != byPath[f.Path] {
			t.Errorf("%s finding for %s names file %q, want %q", f.Check, f.Path, f.FilePID, byPath[f.Path])
		}
	}
}

// TestAuditReportsToleratedDamage: a file that decodes but needed the decoder to
// work around damage is a warn, not an error. It plays, so calling it "corrupt or
// undecodable" would send the user re-ripping a file that is fine to listen to,
// while saying nothing would hide bytes that are rotting.
func TestAuditReportsToleratedDamage(t *testing.T) {
	dir := t.TempDir()
	damaged := filepath.Join(dir, "damaged.wv")
	clean := filepath.Join(dir, "clean.wv")
	for _, p := range []string{damaged, clean} {
		if err := os.WriteFile(p, []byte("real bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st := &fakeStore{files: []model.AuditFileInfo{
		{PID: "f1", Path: []byte(damaged), DisplayPath: damaged, Kind: model.FileAudio, ItemPID: "i1"},
		{PID: "f2", Path: []byte(clean), DisplayPath: clean, Kind: model.FileAudio},
	}}
	probe := func(_ context.Context, p string) ([]string, error) {
		if filepath.Base(p) == "damaged.wv" {
			return []string{"frame sync lost", "3471 trailing bytes dropped"}, nil
		}
		return nil, nil
	}
	rep, err := New(st, nil, probe, nil).Run(context.Background(), Config{
		Only: []model.AuditCheck{model.CheckCorruptAudio}, Integrity: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := findingsFor(rep, model.CheckCorruptAudio)
	if len(got) != 1 {
		t.Fatalf("corrupt findings = %+v, want exactly 1 (the clean file reports nothing)", got)
	}
	if got[0].Severity != model.SeverityWarn {
		t.Errorf("severity = %q, want warn for a file that decodes", got[0].Severity)
	}
	if !strings.Contains(got[0].Message, "frame sync lost") {
		t.Errorf("message %q does not name the first damage", got[0].Message)
	}
	if !strings.Contains(got[0].Message, "and 1 more") {
		t.Errorf("message %q does not count the damage it did not show", got[0].Message)
	}
	if len(got[0].Entities) != 1 {
		t.Errorf("entities = %+v, want the item the file belongs to", got[0].Entities)
	}
}

func TestAuditMissingMBIDRollsUp(t *testing.T) {
	st := &fakeStore{
		missingMBID:  []model.ItemRef{{PID: "i1", Title: "One"}, {PID: "i2", Title: "Two"}},
		missingMBTot: 40,
	}
	rep, err := New(st, nil, nil, nil).Run(context.Background(), Config{
		Only: []model.AuditCheck{model.CheckMissingMBID}, Sample: 2})
	if err != nil {
		t.Fatal(err)
	}
	fs := findingsFor(rep, model.CheckMissingMBID)
	if len(fs) != 3 {
		t.Fatalf("want 2 per-item findings plus a roll-up, got %d: %+v", len(fs), fs)
	}
	if fs[0].Message != "no MusicBrainz identity: One" || len(fs[0].Entities) != 1 {
		t.Errorf("per-item finding = %+v", fs[0])
	}
	if !strings.Contains(fs[2].Message, "40 items") || !strings.Contains(fs[2].Message, "2 shown") {
		t.Errorf("roll-up = %q, want the total and the sample size", fs[2].Message)
	}
	// Info severity keeps an untagged library out of the CLI's error exit.
	if rep.Errors() != 0 {
		t.Errorf("missing_mbid produced %d error findings, want 0", rep.Errors())
	}
}

func TestAuditLibraryConflict(t *testing.T) {
	st := &fakeStore{libs: []*model.Library{
		{PID: "l1", Root: []byte(`C:\Music`), DisplayRoot: `C:\Music`},
		{PID: "l2", Root: []byte(`c:\music`), DisplayRoot: `c:\music`},
		{PID: "l3", Root: []byte(`D:\Books`), DisplayRoot: `D:\Books`},
	}}
	rep, err := New(st, nil, nil, nil).Run(context.Background(), Config{
		Only: []model.AuditCheck{model.CheckLibraryConflict}})
	if err != nil {
		t.Fatal(err)
	}
	fs := findingsFor(rep, model.CheckLibraryConflict)
	if len(fs) != 1 {
		t.Fatalf("want one finding for the colliding pair, got %d: %+v", len(fs), fs)
	}
	// Error only where the platform folds. On a case-sensitive filesystem the two
	// roots are two real directories and a legal configuration, so an error here would
	// fail every audit run on a supported setup.
	wantSev := model.SeverityWarn
	if pathx.FoldsCase {
		wantSev = model.SeverityError
	}
	if fs[0].Severity != wantSev {
		t.Errorf("severity = %q, want %q", fs[0].Severity, wantSev)
	}
	if len(fs[0].Entities) != 2 || fs[0].Entities[0] != "l1" || fs[0].Entities[1] != "l2" {
		t.Errorf("entities = %v, want both colliding pids", fs[0].Entities)
	}
	for _, want := range []string{`C:\Music`, `c:\music`, "case-insensitive", "db reset"} {
		if !strings.Contains(fs[0].Message, want) {
			t.Errorf("message %q does not mention %q", fs[0].Message, want)
		}
	}
}

func TestAuditLibraryConflictCleanCatalog(t *testing.T) {
	st := &fakeStore{libs: []*model.Library{
		{PID: "l1", Root: []byte(`C:\Music`), DisplayRoot: `C:\Music`},
		{PID: "l2", Root: []byte(`D:\Books`), DisplayRoot: `D:\Books`},
	}}
	rep, err := New(st, nil, nil, nil).Run(context.Background(), Config{
		Only: []model.AuditCheck{model.CheckLibraryConflict}})
	if err != nil {
		t.Fatal(err)
	}
	if fs := findingsFor(rep, model.CheckLibraryConflict); len(fs) != 0 {
		t.Errorf("distinct roots reported a conflict: %+v", fs)
	}
}

// Two POSIX roots differing only in an invalid UTF-8 byte are two real directories.
// strings.ToLower decodes each such byte to U+FFFD, which would fold them onto one key
// and report them as one tree under a message printing their identical renderings.
func TestAuditLibraryConflictKeepsRawByteRootsApart(t *testing.T) {
	// Built byte by byte: 0xff and 0xfe are not valid UTF-8, which is the whole point,
	// and a string literal would be normalized before the check ever sees them.
	rootA := append(append([]byte("/mnt/m"), 0xff), []byte("usic")...)
	rootB := append(append([]byte("/mnt/m"), 0xfe), []byte("usic")...)
	// Both roots carry the same lossy UTF-8 rendering, the way the store stores it, so
	// a finding here would print the same name twice and read as "X vs X".
	display := "/mnt/m" + string(utf8.RuneError) + "usic"
	st := &fakeStore{libs: []*model.Library{
		{PID: "l1", Root: rootA, DisplayRoot: display},
		{PID: "l2", Root: rootB, DisplayRoot: display},
	}}
	rep, err := New(st, nil, nil, nil).Run(context.Background(), Config{
		Only: []model.AuditCheck{model.CheckLibraryConflict}})
	if err != nil {
		t.Fatal(err)
	}
	if fs := findingsFor(rep, model.CheckLibraryConflict); len(fs) != 0 {
		t.Errorf("distinct raw-byte roots reported a collision: %+v", fs)
	}
}

// TestAuditCanceledProbeIsNotCorruption pins the cancellation path: a probe cut
// short because the run was canceled must not name the file as corrupt. The old
// behavior emitted a severity-error corrupt finding for whichever healthy file a
// Ctrl-C happened to land on, the one finding a user acts on by re-ripping.
func TestAuditCanceledProbeIsNotCorruption(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "fine.wv")
	if err := os.WriteFile(f, []byte("real bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := &fakeStore{files: []model.AuditFileInfo{
		{PID: "f1", Path: []byte(f), DisplayPath: f, Kind: model.FileAudio},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	probe := func(pctx context.Context, _ string) ([]string, error) {
		cancel()
		return nil, pctx.Err()
	}
	rep, err := New(st, nil, probe, nil).Run(ctx, Config{
		Only: []model.AuditCheck{model.CheckCorruptAudio}, Integrity: true,
	})
	if err == nil {
		t.Fatal("a canceled run should surface the cancellation")
	}
	if rep != nil {
		if got := findingsFor(rep, model.CheckCorruptAudio); len(got) != 0 {
			t.Errorf("corrupt findings = %+v, want none from a canceled probe", got)
		}
	}
}

// TestAuditCorruptDiagnosticsFromEveryWriter: corrupt_audio's cheap half reads the
// verdict whichever writer recorded it, the scan's parse or the analyze pass's decode,
// reports a file both flagged once at the worse severity, and hands the probe half
// every path it reported so no file is decoded again for a verdict already on record.
func TestAuditCorruptDiagnosticsFromEveryWriter(t *testing.T) {
	st := &fakeStore{
		diags: []model.FileDiagnostic{
			{FilePID: "f1", DisplayPath: "/lib/a.flac", Origin: model.OriginAnalyze, Code: model.DiagCorruptAudio,
				Severity: model.SeverityWarn, Detail: "STREAMINFO declares 160000 samples but the frames end at 45056"},
			{FilePID: "f2", DisplayPath: "/lib/b.m4a", Origin: model.OriginAnalyze, Code: model.DiagCorruptAudio,
				Severity: model.SeverityError, Detail: "mp4: fragment sample runs past end of source"},
			{FilePID: "f2", DisplayPath: "/lib/b.m4a", Origin: model.OriginScan, Code: model.DiagCorruptAudio,
				Severity: model.SeverityWarn, Detail: "truncated audio"},
		},
		files: []model.AuditFileInfo{
			{PID: "f1", Path: []byte("/lib/a.flac"), DisplayPath: "/lib/a.flac", Kind: model.FileAudio},
			{PID: "f2", Path: []byte("/lib/b.m4a"), DisplayPath: "/lib/b.m4a", Kind: model.FileAudio},
			{PID: "f3", Path: []byte("/lib/c.wv"), DisplayPath: "/lib/c.wv", Kind: model.FileAudio},
		},
	}
	var probed []string
	probe := func(_ context.Context, p string) ([]string, error) {
		probed = append(probed, filepath.Base(p))
		return nil, os.ErrInvalid
	}
	rep, err := New(st, nil, probe, nil).Run(context.Background(), Config{
		Only: []model.AuditCheck{model.CheckCorruptAudio}, Integrity: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(probed) != 1 || probed[0] != "c.wv" {
		t.Errorf("probed %q, want only c.wv (the others are on record)", probed)
	}
	sev := map[string]model.AuditSeverity{}
	files := map[string]model.PID{"/lib/a.flac": "f1", "/lib/b.m4a": "f2", "/lib/c.wv": "f3"}
	for _, f := range findingsFor(rep, model.CheckCorruptAudio) {
		if _, dup := sev[f.Path]; dup {
			t.Errorf("%s reported twice", f.Path)
		}
		sev[f.Path] = f.Severity
		if f.FilePID != files[f.Path] {
			t.Errorf("%s names file %q, want %q", f.Path, f.FilePID, files[f.Path])
		}
	}
	want := map[string]model.AuditSeverity{
		"/lib/a.flac": model.SeverityWarn, "/lib/b.m4a": model.SeverityError, "/lib/c.wv": model.SeverityError,
	}
	for p, w := range want {
		if sev[p] != w {
			t.Errorf("%s severity = %q, want %q", p, sev[p], w)
		}
	}
	if len(sev) != len(want) {
		t.Errorf("findings by path = %v, want %v", sev, want)
	}
}

// TestAuditDurationMismatch: each sampled file whose header disagrees with its decoded
// audio is a warn finding naming both lengths, and a roll-up names the total when the
// sample was capped.
func TestAuditDurationMismatch(t *testing.T) {
	st := &fakeStore{
		mismatches: []model.FileDurationMismatch{
			{FilePID: "f1", DisplayPath: "/lib/a.mp3", HeaderMS: 168_000, DecodedMS: 240_000},
			{FilePID: "f2", DisplayPath: "/lib/b.mp3", HeaderMS: 60_000, DecodedMS: 100_500},
		},
		mismatchTot: 7,
	}
	rep, err := New(st, nil, nil, nil).Run(context.Background(), Config{
		Only: []model.AuditCheck{model.CheckDurationMismatch}, Sample: 2})
	if err != nil {
		t.Fatal(err)
	}
	fs := findingsFor(rep, model.CheckDurationMismatch)
	if len(fs) != 3 {
		t.Fatalf("want 2 per-file findings plus a roll-up, got %d: %+v", len(fs), fs)
	}
	if fs[0].Severity != model.SeverityWarn || fs[0].Path != "/lib/a.mp3" ||
		!strings.Contains(fs[0].Message, "2:48") || !strings.Contains(fs[0].Message, "4:00") {
		t.Errorf("per-file finding = %+v, want a warn naming both lengths", fs[0])
	}
	if fs[0].FilePID != "f1" || fs[0].HeaderMS != 168_000 || fs[0].DecodedMS != 240_000 {
		t.Errorf("per-file finding = %+v, want f1 at 168000 ms header, 240000 ms decoded", fs[0])
	}
	if !strings.Contains(fs[1].Message, "1:00") || !strings.Contains(fs[1].Message, "1:40") ||
		fs[1].FilePID != "f2" || fs[1].HeaderMS != 60_000 || fs[1].DecodedMS != 100_500 {
		t.Errorf("per-file finding = %+v, want f2 and both lengths", fs[1])
	}
	if !strings.Contains(fs[2].Message, "7 files") || !strings.Contains(fs[2].Message, "2 shown") ||
		fs[2].FilePID != "" || fs[2].HeaderMS != 0 || fs[2].DecodedMS != 0 {
		t.Errorf("roll-up = %+v, want the total and the sample size and no file", fs[2])
	}

	st.mismatchTot = 2
	rep, err = New(st, nil, nil, nil).Run(context.Background(), Config{
		Only: []model.AuditCheck{model.CheckDurationMismatch}, Sample: 5})
	if err != nil {
		t.Fatal(err)
	}
	if fs := findingsFor(rep, model.CheckDurationMismatch); len(fs) != 2 {
		t.Errorf("uncapped sample = %d findings, want 2 and no roll-up", len(fs))
	}
}

// TestAuditDuplicateCopyListsARipCopyOnce: a copy of a rip backs each of its tracks, so it
// comes back under every track, and is one finding all the same.
func TestAuditDuplicateCopyListsARipCopyOnce(t *testing.T) {
	var copies []model.ItemCopies
	for i, title := range []string{"One", "Two", "Three"} {
		copies = append(copies, model.ItemCopies{ItemPID: model.PID("t" + strconv.Itoa(i)), Kind: model.KindTrack, Title: title,
			Files: []model.CopyFile{
				{FilePID: "rip", DisplayPath: "/lib/a/album.flac", Size: 300_000_000, Role: "primary"},
				{FilePID: "ripcopy", DisplayPath: "/lib/b/album.flac", Size: 300_000_000, Role: "alternate", Reason: model.CopySameAudio},
			}})
	}
	rep, err := New(&fakeStore{copies: copies}, nil, nil, nil).Run(context.Background(), Config{Only: []model.AuditCheck{model.CheckDuplicateCopy}})
	if err != nil {
		t.Fatal(err)
	}
	if fs := findingsFor(rep, model.CheckDuplicateCopy); len(fs) != 1 || fs[0].FilePID != "ripcopy" {
		t.Errorf("findings = %+v, want the rip copy once", fs)
	}
}

// TestAuditFileDiagnosticNamesTheFile: a stored diagnostic's finding carries the file it
// was recorded against.
func TestAuditFileDiagnosticNamesTheFile(t *testing.T) {
	st := &fakeStore{diags: []model.FileDiagnostic{{FilePID: "f9", DisplayPath: "/lib/a.ogg",
		Origin: model.OriginScan, Code: model.DiagUnsupportedFormat, Severity: model.SeverityWarn}}}
	rep, err := New(st, nil, nil, nil).Run(context.Background(), Config{Only: []model.AuditCheck{model.CheckFileDiagnostic}})
	if err != nil {
		t.Fatal(err)
	}
	fs := findingsFor(rep, model.CheckFileDiagnostic)
	if len(fs) != 1 || fs[0].FilePID != "f9" || fs[0].Path != "/lib/a.ogg" {
		t.Fatalf("file diagnostic findings = %+v, want one naming f9", fs)
	}
}

// TestAuditFileDiagnosticLeavesCopiesToTheirCheck: the copy codes are the duplicate_copy
// check's to report, so the file_diagnostic check skips them rather than listing each
// copy twice.
func TestAuditFileDiagnosticLeavesCopiesToTheirCheck(t *testing.T) {
	st := &fakeStore{diags: []model.FileDiagnostic{
		{FilePID: "c1", DisplayPath: "/lib/b/song.flac", Origin: model.OriginScan, Code: model.DiagDuplicateCopy,
			Severity: model.SeverityInfo, Detail: "/lib/a/song.flac"},
		{FilePID: "c2", DisplayPath: "/lib/c/song.mp3", Origin: model.OriginScan, Code: model.DiagAlternateEncoding,
			Severity: model.SeverityInfo, Detail: "/lib/a/song.flac"},
		{FilePID: "f9", DisplayPath: "/lib/a.ogg", Origin: model.OriginScan, Code: model.DiagUnsupportedFormat,
			Severity: model.SeverityWarn},
	}}
	rep, err := New(st, nil, nil, nil).Run(context.Background(), Config{Only: []model.AuditCheck{model.CheckFileDiagnostic}})
	if err != nil {
		t.Fatal(err)
	}
	if fs := findingsFor(rep, model.CheckFileDiagnostic); len(fs) != 1 || fs[0].FilePID != "f9" {
		t.Errorf("file diagnostic findings = %+v, want only the unsupported format", fs)
	}
}

func TestClockMS(t *testing.T) {
	for ms, want := range map[int64]string{
		0: "0:00", 59_999: "0:59", 168_000: "2:48", 600_000: "10:00",
		3_600_000: "1:00:00", 3_725_000: "1:02:05", 36_000_000: "10:00:00",
	} {
		if got := clockMS(ms); got != want {
			t.Errorf("clockMS(%d) = %q, want %q", ms, got, want)
		}
	}
}

// TestAuditDuplicateCopy: each alternate file is listed at info under its item, naming
// its path, its size and why it is one, with the primary named beside it; a capped
// sample rolls the rest up.
func TestAuditDuplicateCopy(t *testing.T) {
	st := &fakeStore{copies: []model.ItemCopies{
		{ItemPID: "i1", Kind: model.KindTrack, Title: "Song", Files: []model.CopyFile{
			{FilePID: "p1", DisplayPath: "/lib/a/song.flac", Size: 30_000_000, Role: "primary"},
			{FilePID: "c1", DisplayPath: "/lib/b/song.flac", Size: 30_000_000, Role: "alternate", Reason: model.CopySameAudio},
			{FilePID: "c2", DisplayPath: "/lib/c/song.mp3", Size: 5_200_000, Role: "alternate", Reason: model.CopyOtherEncoding},
		}},
		{ItemPID: "i2", Kind: model.KindTrack, Title: "Other", Files: []model.CopyFile{
			{FilePID: "p2", DisplayPath: "/lib/a/other.mp3", Size: 4_000_000, Role: "primary"},
			{FilePID: "c3", DisplayPath: "/lib/b/other.mp3", Size: 4_000_000, Role: "alternate", Reason: model.CopySameAudio},
		}},
	}}
	rep, err := New(st, nil, nil, nil).Run(context.Background(), Config{Only: []model.AuditCheck{model.CheckDuplicateCopy}})
	if err != nil {
		t.Fatal(err)
	}
	fs := findingsFor(rep, model.CheckDuplicateCopy)
	if len(fs) != 3 {
		t.Fatalf("findings = %+v, want one per alternate", fs)
	}
	for _, f := range fs {
		if f.Severity != model.SeverityInfo || len(f.Entities) != 1 || f.FilePID == "" || f.Path == "" {
			t.Errorf("finding = %+v, want info naming its item and file", f)
		}
	}
	if f := fs[1]; f.FilePID != "c2" || f.Entities[0] != "i1" ||
		!strings.Contains(f.Message, "other encoding") || !strings.Contains(f.Message, "5.2 MB") ||
		!strings.Contains(f.Message, "/lib/c/song.mp3") || !strings.Contains(f.Message, "/lib/a/song.flac") {
		t.Errorf("encoding finding = %+v, want the reason, size, path and the primary", f)
	}
	if !strings.Contains(fs[0].Message, "same audio") {
		t.Errorf("copy finding = %+v, want the same-audio reason", fs[0])
	}
	rep, err = New(st, nil, nil, nil).Run(context.Background(), Config{Only: []model.AuditCheck{model.CheckDuplicateCopy}, Sample: 2})
	if err != nil {
		t.Fatal(err)
	}
	if fs := findingsFor(rep, model.CheckDuplicateCopy); len(fs) != 3 || !strings.Contains(fs[2].Message, "3 copies on 2 items (2 shown)") {
		t.Errorf("capped = %+v, want two and a roll-up", fs)
	}
}
