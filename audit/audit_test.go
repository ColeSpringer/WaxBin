package audit

import (
	"context"
	"os"
	"path/filepath"
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

func TestAuditFileChecks(t *testing.T) {
	st := &fakeStore{files: []model.AuditFileInfo{
		{PID: "f1", Path: []byte("/lib/al/song.flac"), DisplayPath: "/lib/al/song.flac", Kind: model.FileAudio},
		{PID: "f2", Path: []byte("/lib/al/song.lrc"), DisplayPath: "/lib/al/song.lrc", Kind: model.FileLyrics},         // not orphan (audio in dir)
		{PID: "f3", Path: []byte("/lib/stray/notes.lrc"), DisplayPath: "/lib/stray/notes.lrc", Kind: model.FileLyrics}, // orphan
		{PID: "f4", Path: []byte("/lib/al/what?.flac"), DisplayPath: "/lib/al/what?.flac", Kind: model.FileAudio},      // bad name
		{PID: "f5", Path: []byte("/lib/x/Track.flac"), DisplayPath: "/lib/x/Track.flac", Kind: model.FileAudio},
		{PID: "f6", Path: []byte("/lib/x/track.flac"), DisplayPath: "/lib/x/track.flac", Kind: model.FileAudio}, // case conflict with f5
	}}
	rep, err := New(st, nil, nil, nil).Run(context.Background(), Config{Only: []model.AuditCheck{
		model.CheckBadFilename, model.CheckOrphanSidecar, model.CheckPathConflict,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := findingsFor(rep, model.CheckBadFilename); len(got) != 1 || got[0].Path != "/lib/al/what?.flac" {
		t.Errorf("bad filename findings = %+v", got)
	}
	if got := findingsFor(rep, model.CheckOrphanSidecar); len(got) != 1 || got[0].Path != "/lib/stray/notes.lrc" {
		t.Errorf("orphan sidecar findings = %+v", got)
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
	probeFail := func(_ context.Context, p string) error {
		if filepath.Base(p) == "good.flac" {
			return nil
		}
		return os.ErrInvalid
	}
	rep, err := New(st, identity.ContentHash, probeFail, nil).Run(context.Background(), Config{
		Only:      []model.AuditCheck{model.CheckIntegrity, model.CheckCorruptAudio},
		Integrity: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// bitrot (hash mismatch) + gone (missing) = 2 integrity errors.
	if got := findingsFor(rep, model.CheckIntegrity); len(got) != 2 {
		t.Errorf("integrity findings = %+v, want 2", got)
	}
	if rep.FilesChecked != 3 {
		t.Errorf("FilesChecked = %d, want 3", rep.FilesChecked)
	}
	// good.flac probes clean; the other two (a missing file and bitrot) fail the probe.
	if got := findingsFor(rep, model.CheckCorruptAudio); len(got) != 2 {
		t.Errorf("corrupt findings = %+v, want 2", got)
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
	probe := func(pctx context.Context, _ string) error {
		cancel()
		return pctx.Err()
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
