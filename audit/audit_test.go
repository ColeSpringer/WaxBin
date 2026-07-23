package audit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
)

// fakeStore is a hand-rolled audit.Store for exercising the auditor logic without
// a real database.
type fakeStore struct {
	dupArtists []model.DuplicateSet
	dupGenres  []model.DuplicateSet
	dupAlbums  []model.DuplicateSet
	splits     []model.SplitAlbum
	inconsist  []model.AlbumIssue
	missingArt []model.ItemRef
	missingTot int
	missingRG  int
	files      []model.AuditFileInfo
	pods       []*model.Podcast
	drift      model.DerivedDrift
	diags      []model.FileDiagnostic
	diagStale  int
	diagTotal  int
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
func (f *fakeStore) CountItemsMissingReplayGain(context.Context) (int, error) {
	return f.missingRG, nil
}
func (f *fakeStore) AuditFiles(context.Context) ([]model.AuditFileInfo, error) { return f.files, nil }
func (f *fakeStore) Podcasts(context.Context) ([]*model.Podcast, error)        { return f.pods, nil }
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
