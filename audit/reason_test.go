package audit

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// TestDuplicateFindingsCarryTheirReasonAndMembers: a duplicate finding carries its reason
// and its members with their names and track counts, survivor first, and its message is
// built from the reason (naming the id a shared-MBID pair shares).
func TestDuplicateFindingsCarryTheirReasonAndMembers(t *testing.T) {
	t.Parallel()
	members := []model.DuplicateMember{{PID: "rg1", Name: "Hits", TrackCount: 12}, {PID: "rg2", Name: "Hits!", TrackCount: 3}}
	st := &fakeStore{dupRGs: []model.DuplicateSet{{EntityType: model.MergeReleaseGroup, Reason: model.ReasonSharedMBID,
		MBID: "0f1e2d3c-0000-4000-8000-000000000001", Members: members}}}
	rep, err := New(st, nil, nil, nil).Run(context.Background(), Config{Only: []model.AuditCheck{model.CheckDuplicateReleaseGroup}})
	if err != nil {
		t.Fatal(err)
	}
	fs := findingsFor(rep, model.CheckDuplicateReleaseGroup)
	if len(fs) != 1 {
		t.Fatalf("findings = %+v, want one", fs)
	}
	f := fs[0]
	if f.Reason != string(model.ReasonSharedMBID) || !reflect.DeepEqual(f.Members, members) {
		t.Errorf("finding = %+v, want the shared-MBID reason and both members", f)
	}
	if !strings.Contains(f.Message, string(model.ReasonSharedMBID)+" 0f1e2d3c-0000-4000-8000-000000000001") {
		t.Errorf("message = %q, want it to say the reason and name the id", f.Message)
	}
}

// TestOverlappingReasonsKeepTheMBIDPairing: one pair found by its MBID and by its collation
// key is one finding, the MBID's, which is the stronger evidence.
func TestOverlappingReasonsKeepTheMBIDPairing(t *testing.T) {
	t.Parallel()
	pair := []model.DuplicateMember{{PID: "a1", Name: "Beatles", TrackCount: 5}, {PID: "a2", Name: "The Beatles", TrackCount: 2}}
	st := &fakeStore{dupArtists: []model.DuplicateSet{
		{EntityType: model.MergeArtist, Reason: model.ReasonSharedMBID, MBID: "b10bbbfc-cf9e-42e0-be17-e2c3e1d2600d", Members: pair},
		{EntityType: model.MergeArtist, Reason: model.ReasonSameCollationKey, Members: pair},
	}}
	rep, err := New(st, nil, nil, nil).Run(context.Background(), Config{Only: []model.AuditCheck{model.CheckDuplicateArtist}})
	if err != nil {
		t.Fatal(err)
	}
	fs := findingsFor(rep, model.CheckDuplicateArtist)
	if len(fs) != 1 || fs[0].Reason != string(model.ReasonSharedMBID) {
		t.Fatalf("findings = %+v, want the one MBID pairing", fs)
	}
}

// TestEveryDiscriminatedFindingHasAReason: each check whose findings have more than one
// cause says which in Reason, and a capped roll-up says how many it stands for in More.
func TestEveryDiscriminatedFindingHasAReason(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	kept, rotted := filepath.Join(dir, "kept.flac"), filepath.Join(dir, "rotted.flac")
	for _, p := range []string{kept, rotted} {
		if err := os.WriteFile(p, []byte("audio"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files := []model.AuditFileInfo{
		{PID: "f1", Path: []byte(rotted), DisplayPath: rotted, Kind: model.FileAudio, ContentHash: "stale"},
		{PID: "f2", Path: []byte(filepath.Join(dir, "gone.flac")), DisplayPath: "gone.flac", Kind: model.FileAudio, ContentHash: "x"},
		{PID: "f3", Path: []byte(kept), DisplayPath: kept, Kind: model.FileAudio},
	}
	hash := func(p string) (string, error) {
		if _, err := os.Stat(p); err != nil {
			return "", err
		}
		return "fresh", nil
	}
	probe := func(_ context.Context, p string) ([]string, error) {
		switch filepath.Base(p) {
		case "kept.flac":
			return []string{"frame sync lost"}, nil
		case "rotted.flac":
			return nil, os.ErrInvalid
		}
		return nil, nil
	}
	for _, tc := range []struct {
		name  string
		check model.AuditCheck
		st    *fakeStore
		want  []string
	}{
		{name: "integrity", check: model.CheckIntegrity, st: &fakeStore{files: files},
			want: []string{string(model.IntegrityHashChanged), string(model.IntegrityUnreadable)}},
		{name: "corrupt_audio", check: model.CheckCorruptAudio, st: &fakeStore{files: files},
			want: []string{string(model.CorruptUndecodable), string(model.CorruptDamaged)}},
		{name: "corrupt_audio diagnostics", check: model.CheckCorruptAudio, st: &fakeStore{diags: []model.FileDiagnostic{
			{FilePID: "d1", DisplayPath: "/lib/a.flac", Code: model.DiagCorruptAudio, Severity: model.SeverityError},
			{FilePID: "d2", DisplayPath: "/lib/b.flac", Code: model.DiagCorruptAudio, Severity: model.SeverityWarn},
		}}, want: []string{string(model.CorruptUndecodable), string(model.CorruptDamaged)}},
		{name: "file_diagnostic", check: model.CheckFileDiagnostic, st: &fakeStore{diags: []model.FileDiagnostic{
			{FilePID: "d3", DisplayPath: "/lib/c.flac", Code: model.DiagLegacyOnlyTags, Severity: model.SeverityWarn},
		}}, want: []string{string(model.DiagLegacyOnlyTags)}},
		{name: "duplicate_genre", check: model.CheckDuplicateGenre,
			st: &fakeStore{dupGenres: []model.DuplicateSet{{EntityType: model.MergeGenre, Reason: model.ReasonSameCollationKey,
				Members: []model.DuplicateMember{{PID: "g1", Name: "Hip Hop"}, {PID: "g2", Name: "Hip-Hop"}}}}},
			want: []string{string(model.ReasonSameCollationKey)}},
		{name: "duplicate_album", check: model.CheckDuplicateAlbum,
			st: &fakeStore{dupAlbums: []model.DuplicateSet{{EntityType: model.MergeAlbum, Reason: model.ReasonSameAlbumName,
				Members: []model.DuplicateMember{{PID: "al1", Name: "Hits"}, {PID: "al2", Name: "Hits"}}}}},
			want: []string{string(model.ReasonSameAlbumName)}},
		{name: "invalid_feed", check: model.CheckInvalidFeed,
			st: &fakeStore{pods: []*model.Podcast{
				{PID: "p1", Title: "Bad", FeedURL: "ftp://host/feed", EpisodeCount: 3},
				{PID: "p2", Title: "Empty", FeedURL: "https://host/feed"},
			}},
			want: []string{string(model.FeedBadURL), string(model.FeedNoEpisodes)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rep, err := New(tc.st, hash, probe, nil).Run(ctx, Config{Only: []model.AuditCheck{tc.check}, Integrity: true})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, f := range findingsFor(rep, tc.check) {
				got = append(got, f.Reason)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("reasons = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestACappedRollUpSaysHowManyMoreItStandsFor: a check capped at its sample ends with a
// roll-up finding that says how many findings beyond those shown it stands for, which no
// other finding does.
func TestACappedRollUpSaysHowManyMoreItStandsFor(t *testing.T) {
	t.Parallel()
	var diags []model.FileDiagnostic
	for i := range 5 {
		diags = append(diags, model.FileDiagnostic{FilePID: model.PID("f" + string(rune('a'+i))), DisplayPath: "/lib/" + string(rune('a'+i)),
			Code: model.DiagLegacyOnlyTags, Severity: model.SeverityWarn})
	}
	missing := []model.ItemRef{{PID: "i1", Title: "One"}, {PID: "i2", Title: "Two"}}
	rep, err := New(&fakeStore{diags: diags, missingArt: missing, missingTot: 7}, nil, nil, nil).Run(context.Background(),
		Config{Only: []model.AuditCheck{model.CheckFileDiagnostic, model.CheckMissingArt}, Sample: 2})
	if err != nil {
		t.Fatal(err)
	}
	for check, want := range map[model.AuditCheck]int{model.CheckFileDiagnostic: 3, model.CheckMissingArt: 5} {
		fs := findingsFor(rep, check)
		if len(fs) != 3 || fs[2].More != want || fs[0].More != 0 || fs[1].More != 0 {
			t.Errorf("%s findings = %+v, want two shown and a roll-up of %d more", check, fs, want)
		}
	}
}
