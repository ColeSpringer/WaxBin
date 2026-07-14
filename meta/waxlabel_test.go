package meta

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	waxlabel "github.com/colespringer/waxlabel"
	"github.com/colespringer/waxlabel/tag"
)

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestReadProjectsTags verifies the adapter projects WaxLabel's canonical tags
// and stream properties into model.Tags.
func TestReadProjectsTags(t *testing.T) {
	p := writeTemp(t, "song.mp3", testaudio.BuildMP3("Midnight Drive", "The Foobars", "Night Moves", 3))
	fm, err := NewReader().Read(context.Background(), p)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	tg := fm.Tags
	if tg.Title != "Midnight Drive" || tg.Artist != "The Foobars" || tg.Album != "Night Moves" || tg.TrackNo != 3 {
		t.Fatalf("tag projection wrong: %+v", tg)
	}
	if tg.Codec != "mp3" {
		t.Errorf("codec = %q, want normalized lowercase mp3", tg.Codec)
	}
	if tg.DurationMS <= 0 || tg.SampleRate != 44100 {
		t.Errorf("audio properties not read: dur=%d rate=%d", tg.DurationMS, tg.SampleRate)
	}
	if fm.EssenceHash == "" {
		t.Error("essence hash empty for a valid MP3")
	}
}

// TestEssenceStableAcrossRetag verifies the essence hash is tag-independent: two
// files with identical audio but different tags hash the same, while different
// audio hashes differently.
func TestEssenceStableAcrossRetag(t *testing.T) {
	ctx := context.Background()
	audio := testaudio.DefaultAudio()

	a := writeTemp(t, "a.mp3", testaudio.BuildMP3WithAudio("Title A", "Artist A", "Album A", 1, audio))
	b := writeTemp(t, "b.mp3", testaudio.BuildMP3WithAudio("Totally Different", "Other", "Else", 9, audio))
	c := writeTemp(t, "c.mp3", testaudio.BuildMP3WithAudio("Title A", "Artist A", "Album A", 1, testaudio.AudioWithSeed(0x40)))

	read := func(path string) string {
		fm, err := NewReader().Read(ctx, path)
		if err != nil {
			t.Fatalf("Read %s: %v", path, err)
		}
		if fm.EssenceHash == "" {
			t.Fatalf("empty essence for %s", path)
		}
		return fm.EssenceHash
	}

	if read(a) != read(b) {
		t.Error("essence changed across a pure retag; it must be tag-independent")
	}
	if read(a) == read(c) {
		t.Error("essence matched despite different audio; it must depend on the audio")
	}
}

// TestReadMissingFile surfaces an I/O error rather than panicking.
func TestReadMissingFile(t *testing.T) {
	if _, err := NewReader().Read(context.Background(), filepath.Join(t.TempDir(), "nope.mp3")); err == nil {
		t.Fatal("expected an error reading a missing file")
	}
}

// TestReadToleratesUnsupportedFormat verifies a format WaxLabel cannot parse is
// still cataloged with a filename title and content-hash essence.
func TestReadToleratesUnsupportedFormat(t *testing.T) {
	p := writeTemp(t, "track.wma", []byte("not a recognized audio container, just bytes"))
	fm, err := NewReader().Read(context.Background(), p)
	if err != nil {
		t.Fatalf("Read should tolerate an unsupported format, got %v", err)
	}
	if fm.Tags.Title != "track" {
		t.Errorf("title = %q, want the filename-derived 'track'", fm.Tags.Title)
	}
	if fm.EssenceHash != "" {
		t.Errorf("essence = %q, want empty so the scanner falls back to the content hash", fm.EssenceHash)
	}
	if fm.Tags.Codec != "wma" {
		t.Errorf("codec = %q, want the extension-derived 'wma' for display", fm.Tags.Codec)
	}
}

// TestReadWAVEssence verifies a pure-Go-decodable WAV gets a real essence hash
// and the lowercase pcm codec the analyze registry selects on.
func TestReadWAVEssence(t *testing.T) {
	wav := testaudio.EncodeWAV16(22050, testaudio.RichSignal(22050, 2, testaudio.MusicalPartials, 1))
	p := writeTemp(t, "tone.wav", wav)
	fm, err := NewReader().Read(context.Background(), p)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if fm.Tags.Codec != "pcm" {
		t.Errorf("WAV codec = %q, want pcm", fm.Tags.Codec)
	}
	if fm.EssenceHash == "" {
		t.Error("WAV essence hash empty")
	}
}

// TestReadCommentTakesFirst pins the projection of WaxLabel's multi-valued COMMENT
// onto WaxBin's single Comment field: the first value wins and the rest are dropped.
// Formats that carry several comments (Vorbis, AIFF ANNO, MP4 cmt, ID3 COMM) would
// otherwise have no defined answer for the one column the catalog stores.
func TestReadCommentTakesFirst(t *testing.T) {
	ctx := context.Background()
	p := writeTemp(t, "commented.mp3", testaudio.BuildMP3("T", "A", "Al", 1))
	if _, err := NewWriter().Apply(ctx, p, []TagEdit{{Key: "COMMENT", Values: []string{"  first  ", "second"}}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	fm, err := NewReader().Read(ctx, p)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if fm.Tags.Comment != "first" {
		t.Errorf("Comment = %q, want trimmed first value %q", fm.Tags.Comment, "first")
	}
}

// TestReadLegacyOnlyTagsFallback covers the live defect: WaxLabel builds the
// canonical tag set from the authoritative container alone, so an MP3 carrying only
// an ID3v1 trailer used to catalog with no artist, no album, and a filename-derived
// title.
func TestReadLegacyOnlyTagsFallback(t *testing.T) {
	raw := testaudio.AppendID3v1(testaudio.DefaultAudio(), "V1 Title", "V1 Only Artist", "V1 Album")
	p := writeTemp(t, "v1only.mp3", raw)

	fm, err := NewReader().Read(context.Background(), p)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if fm.Tags.Title != "V1 Title" {
		t.Errorf("Title = %q, want the ID3v1 title to beat the filename fallback", fm.Tags.Title)
	}
	if fm.Tags.Artist != "V1 Only Artist" {
		t.Errorf("Artist = %q, want the ID3v1 artist", fm.Tags.Artist)
	}
	if fm.Tags.Album != "V1 Album" {
		t.Errorf("Album = %q, want the ID3v1 album", fm.Tags.Album)
	}
	// Artist and Artists are filled together so the display field and the entity
	// list cannot disagree.
	if len(fm.Tags.Artists) != 1 || fm.Tags.Artists[0] != "V1 Only Artist" {
		t.Errorf("Artists = %v, want it filled alongside Artist", fm.Tags.Artists)
	}
}

// TestLegacyFallbackNeverChangesIdentity is the assertion that guards the whole
// item graph. identity.TrackKey prefers "mbid:"+MBID over "essence:"+hash, and
// upsertItem creates a NEW item on an identity miss, so a legacy MBID promoted into
// Tags.MBID would re-key an existing item on the next full scan and orphan its PID,
// play state, ratings, and provenance. The fallback must fill display fields only.
func TestLegacyFallbackNeverChangesIdentity(t *testing.T) {
	raw := testaudio.AppendID3v1(testaudio.DefaultAudio(), "V1 Title", "V1 Only Artist", "V1 Album")
	p := writeTemp(t, "v1only.mp3", raw)

	fm, err := NewReader().Read(context.Background(), p)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if fm.Tags.MBID != "" {
		t.Errorf("MBID = %q, want empty: a legacy container must never supply an identity key", fm.Tags.MBID)
	}
	if fm.Tags.IsAudiobook {
		t.Error("IsAudiobook = true: a legacy container must never change an item's kind")
	}
	if key := identity.TrackKey(fm.Tags.MBID, fm.EssenceHash); !strings.HasPrefix(key, "essence:") {
		t.Errorf("identity key = %q, want an essence: key", key)
	}
}

// TestLegacyFallbackAllowlistExcludesIdentityKeys pins the exclusion as a rule
// rather than as a property of whichever fixture happens to be handy: ID3v1 cannot
// express an MBID, so no MP3 fixture can catch a regression here.
func TestLegacyFallbackAllowlistExcludesIdentityKeys(t *testing.T) {
	forbidden := []tag.Key{
		tag.MBRecordingID, tag.MBReleaseID, tag.MBReleaseGroupID, tag.MBArtistID,
		tag.MBAlbumArtistID, tag.MBReleaseTrackID, tag.MBWorkID, tag.MBDiscID,
		tag.ISRC, tag.Narrator, tag.MediaType,
	}
	for _, k := range forbidden {
		if legacyFallbackKeys[k] {
			t.Errorf("legacyFallbackKeys[%s] = true; it re-keys an item's identity or kind and must stay excluded", k)
		}
	}
}

// TestLegacyFallbackIsFillOnly verifies a legacy container never overrides a
// canonical value: the fallback exists to fill gaps, not to arbitrate.
func TestLegacyFallbackIsFillOnly(t *testing.T) {
	base := testaudio.BuildMP3("V2 Title", "V2 Artist", "V2 Album", 3)
	p := writeTemp(t, "both.mp3", testaudio.AppendID3v1(base, "V1 Title", "V1 Artist", "V1 Album"))

	fm, err := NewReader().Read(context.Background(), p)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if fm.Tags.Title != "V2 Title" || fm.Tags.Artist != "V2 Artist" || fm.Tags.Album != "V2 Album" {
		t.Errorf("ID3v1 overrode the canonical ID3v2 set: %+v", fm.Tags)
	}
}

// TestBookFieldsTrimUntrimmedMediaType pins the trap in reading the typed
// projection: tag.Project does not trim, and MediaType is compared exactly, so a
// MEDIATYPE of " 2" would silently stop classifying the file as a book. No narrator
// is set here, so mediaType is the only thing that can classify it.
func TestBookFieldsTrimUntrimmedMediaType(t *testing.T) {
	raw := testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "A Book", Artist: "Author", Album: "A Book",
		TXXX: []testaudio.TXXXFrame{{Desc: "MEDIATYPE", Value: " 2"}},
	})
	p := writeTemp(t, "book.mp3", raw)

	fm, err := NewReader().Read(context.Background(), p)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !fm.Tags.IsAudiobook {
		t.Error("IsAudiobook = false for MEDIATYPE=\" 2\"; the projection is untrimmed and must be trimmed at the use site")
	}
}

// TestBookFieldsTrimUntrimmedNarrator covers the other untrimmed use site: a
// whitespace-only NARRATOR must not classify a file as a book.
func TestBookFieldsTrimUntrimmedNarrator(t *testing.T) {
	raw := testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "A Song", Artist: "Band", Album: "An Album",
		TXXX: []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "   "}},
	})
	p := writeTemp(t, "song.mp3", raw)

	fm, err := NewReader().Read(context.Background(), p)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if fm.Tags.IsAudiobook {
		t.Error("IsAudiobook = true for a whitespace-only NARRATOR; the value must be trimmed before the non-empty test")
	}
}

func TestParseSeries(t *testing.T) {
	cases := []struct {
		in, name, seq string
	}{
		{"Stormlight Archive #2", "Stormlight Archive", "2"},
		{"Wheel of Time, Book 3", "Wheel of Time", "3"},
		{"Discworld #1.5", "Discworld", "1.5"},
		{"The Expanse, Volume 4", "The Expanse", "4"},
		{"Area 51", "Area 51", ""}, // a trailing number with no marker stays in the name
		{"Just A Series", "Just A Series", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		name, seq := parseSeries(c.in)
		if name != c.name || seq != c.seq {
			t.Errorf("parseSeries(%q) = (%q,%q), want (%q,%q)", c.in, name, seq, c.name, c.seq)
		}
	}
}

func TestParseAbridged(t *testing.T) {
	if a, ed := parseAbridged("The Hobbit (Unabridged)", "", ""); a == nil || *a || ed != "Unabridged" {
		t.Errorf("unabridged: got (%v,%q), want (false,Unabridged)", a, ed)
	}
	if a, ed := parseAbridged("Some Book [Abridged]", "", ""); a == nil || !*a || ed != "Abridged" {
		t.Errorf("abridged: got (%v,%q), want (true,Abridged)", a, ed)
	}
	if a, ed := parseAbridged("Plain Title", "", ""); a != nil || ed != "" {
		t.Errorf("unmarked: got (%v,%q), want (nil,\"\")", a, ed)
	}
}

func TestSplitCredits(t *testing.T) {
	// Splits on the unambiguous delimiters ; / &.
	got := SplitCredits("Neil Gaiman & Terry Pratchett")
	if len(got) != 2 || got[0] != "Neil Gaiman" || got[1] != "Terry Pratchett" {
		t.Errorf("SplitCredits(&) = %v", got)
	}
	if got := SplitCredits("A; B / C"); len(got) != 3 {
		t.Errorf("SplitCredits(;/) = %v, want 3", got)
	}
	// Does NOT split a "Last, First" name on the comma, nor an entity containing "and".
	if got := SplitCredits("Tolkien, J.R.R."); len(got) != 1 {
		t.Errorf("SplitCredits(Last, First) = %v, want 1 (comma is not a split)", got)
	}
	if got := SplitCredits("Simon and Schuster"); len(got) != 1 {
		t.Errorf("SplitCredits(and) = %v, want 1 (\"and\" is not a split)", got)
	}
	if SplitCredits("  ") != nil {
		t.Error("blank should split to nil")
	}
}

func TestParseAbridgedBracketedOnly(t *testing.T) {
	// The bracketed marker is detected...
	if a, _ := parseAbridged("The Hobbit (Unabridged)", "", ""); a == nil || *a {
		t.Error("(Unabridged) should yield abridged=false")
	}
	if a, _ := parseAbridged("Dune [Abridged]", "", ""); a == nil || !*a {
		t.Error("[Abridged] should yield abridged=true")
	}
	// ...but a bare word in real prose is NOT (no false positive, no key pollution).
	for _, s := range []string{
		"An Abridged History of Time", // a genuine title containing the word
		"unabridgedness study", "Bridged Worlds",
	} {
		if a, ed := parseAbridged(s, "", ""); a != nil || ed != "" {
			t.Errorf("parseAbridged(%q) = (%v,%q), want no match", s, a, ed)
		}
	}
}

// TestReadProjectsAcquisitionTags verifies the acquisition keys project into the
// model, and, the part that matters, that an acquisition date never reaches Year.
// Year is the release year of the WORK: it drives the year: field, the year facet,
// sort keys, and organize's {year} path token, so folding an acquisition date in
// would catalog a 1975 album downloaded in 2019 as year=2019.
func TestReadProjectsAcquisitionTags(t *testing.T) {
	raw := testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Old Song", Artist: "Band", Album: "Old Album", Year: 1975,
		TXXX: []testaudio.TXXXFrame{
			{Desc: "SOURCE_URL", Value: "https://example.test/track/9"},
			{Desc: "SOURCE_ID", Value: "9"},
			{Desc: "ACQUISITION_DATE", Value: "2019-05-03"},
		},
	})
	p := writeTemp(t, "dl.mp3", raw)

	fm, err := NewReader().Read(context.Background(), p)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	a := fm.Tags.Acquisition
	if a.SourceURL != "https://example.test/track/9" || a.SourceID != "9" {
		t.Errorf("acquisition = %+v", a)
	}
	if !a.Present() {
		t.Error("Present() = false with a source URL and ID")
	}
	want := time.Date(2019, 5, 3, 0, 0, 0, 0, time.UTC).UnixNano()
	if a.AcquiredAt != want {
		t.Errorf("AcquiredAt = %d, want %d (2019-05-03 UTC)", a.AcquiredAt, want)
	}
	if fm.Tags.Year != 1975 {
		t.Errorf("Year = %d, want the release year 1975: an acquisition date must never reach Year", fm.Tags.Year)
	}
}

// TestParseAcquiredAt covers the partial-date precisions WaxLabel accepts and the
// degradation rule: an unusable value yields 0, the store's "stamp it for me"
// sentinel, rather than a confidently wrong date.
func TestParseAcquiredAt(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"2019-05-03", time.Date(2019, 5, 3, 0, 0, 0, 0, time.UTC).UnixNano()},
		{"2019-05", time.Date(2019, 5, 1, 0, 0, 0, 0, time.UTC).UnixNano()},
		{"2019", time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()},
		{"  2019-05-03  ", time.Date(2019, 5, 3, 0, 0, 0, 0, time.UTC).UnixNano()},
		{"", 0},
		{"not a date", 0},
		{"2019-5-3", 0},             // not zero-padded canonical form
		{"2019-02-31", 0},           // calendar-checked
		{"0000-01-01", 0},           // year 0000 is not meaningful
		{"2019-05-03T10:00:00Z", 0}, // not a partial-date form
	}
	for _, c := range cases {
		if got := parseAcquiredAt(c.in); got != c.want {
			t.Errorf("parseAcquiredAt(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestReadDiagnosesUnsupportedFormat verifies the unsupported-format branch now says
// why a file cataloged with no tags, instead of swallowing it silently.
func TestReadDiagnosesUnsupportedFormat(t *testing.T) {
	p := writeTemp(t, "track.wma", []byte("not a recognized audio container, just bytes"))
	fm, err := NewReader().Read(context.Background(), p)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(fm.Diagnostics) != 1 || fm.Diagnostics[0].Code != model.DiagUnsupportedFormat {
		t.Fatalf("diagnostics = %+v, want one unsupported_format", fm.Diagnostics)
	}
	if fm.Diagnostics[0].Severity != model.SeverityInfo {
		t.Errorf("severity = %q, want info: the file is cataloged, just untagged", fm.Diagnostics[0].Severity)
	}
}

// TestReadDiagnosesNoAudioFrames pins the severity rule for the ambiguous case. The
// upstream code's own doc says a no-audio-frames file "may be tag-only or truncated",
// so an unconditional error would permanently flag a legitimately tag-only MP3 as
// broken. It must be warn.
func TestReadDiagnosesNoAudioFrames(t *testing.T) {
	// An ID3v2-tagged .mp3 whose essence region holds non-MPEG bytes.
	raw := testaudio.BuildMP3WithAudio("T", "A", "Al", 1, []byte("this is plainly not mpeg audio at all"))
	p := writeTemp(t, "notaudio.mp3", raw)

	fm, err := NewReader().Read(context.Background(), p)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var got *model.FileDiagnostic
	for i := range fm.Diagnostics {
		if fm.Diagnostics[i].Code == model.DiagCorruptAudio {
			got = &fm.Diagnostics[i]
		}
	}
	if got == nil {
		t.Fatalf("no corrupt_audio diagnostic; diagnostics = %+v", fm.Diagnostics)
	}
	if got.Severity != model.SeverityWarn {
		t.Errorf("severity = %q, want warn: the condition is ambiguous (tag-only OR truncated), "+
			"so an error would permanently flag a legitimately tag-only MP3", got.Severity)
	}
}

// TestReadDoesNotFoldWarningsWholesale is the negative control that keeps the
// diagnostic vocabulary curated. A trailing ID3v1 tag makes WaxLabel emit a warning,
// and conditions like it fire on most pre-2005 rips. Folding the warning set in
// wholesale would store millions of rows and leave the audit reporting problems on a
// healthy library. Only the named codes may become diagnostics.
func TestReadDoesNotFoldWarningsWholesale(t *testing.T) {
	// This file provably carries a trailing-id3v1 warning upstream.
	raw := testaudio.AppendID3v1(testaudio.BuildMP3("T", "A", "Al", 1), "V1 T", "V1 A", "V1 Al")
	p := writeTemp(t, "healthy.mp3", raw)

	fm, err := NewReader().Read(context.Background(), p)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	for _, d := range fm.Diagnostics {
		if d.Code == model.DiagCorruptAudio {
			t.Errorf("healthy file got a corrupt_audio diagnostic: %+v", d)
		}
	}
}

// TestCapDetail verifies the length bound and that truncation lands on a rune
// boundary. Upstream sanitizes a warning message but does not bound it, and a
// message can embed a file-derived snippet, and sanitizing defends against
// injection rather than against size.
func TestCapDetail(t *testing.T) {
	if got := capDetail("short"); got != "short" {
		t.Errorf("capDetail(short) = %q", got)
	}
	long := strings.Repeat("a", 1<<20)
	if got := capDetail(long); len(got) != maxDetailBytes {
		t.Errorf("len = %d, want %d", len(got), maxDetailBytes)
	}
	// Multi-byte runes: the cap must not slice one in half.
	multi := strings.Repeat("é", 1<<20) // 2 bytes each
	got := capDetail(multi)
	if len(got) > maxDetailBytes {
		t.Errorf("len = %d, want <= %d", len(got), maxDetailBytes)
	}
	if !utf8.ValidString(got) {
		t.Error("capped detail is not valid UTF-8")
	}
	emoji := strings.Repeat("🎵", 1<<20) // 4 bytes each; 512 is not a multiple of 4
	got = capDetail(emoji)
	if !utf8.ValidString(got) {
		t.Error("capped emoji detail is not valid UTF-8")
	}
	if len(got) > maxDetailBytes {
		t.Errorf("len = %d, want <= %d", len(got), maxDetailBytes)
	}
}

// TestParseLRCReportsDropped covers the signal the reporting parser exists for:
// telling a partly-broken sidecar from a plain-text one.
func TestParseLRCReportsDropped(t *testing.T) {
	cases := []struct {
		name        string
		text        string
		wantLines   int
		wantDropped int
		wantPartial bool
	}{
		{
			name:        "clean",
			text:        "[00:00.00]one\n[00:01.00]two\n",
			wantLines:   2,
			wantDropped: 0,
			wantPartial: false,
		},
		{
			// The signal: some lines timed, some not.
			name:        "partial",
			text:        "[00:00.00]good\n[bogus]bad timestamp\nuntimed text\n",
			wantLines:   1,
			wantDropped: 2,
			wantPartial: true,
		},
		{
			// The negative that carries the weight. A fully untimed .lrc reports every
			// line as dropped, and that is the ordinary case of plain text rather than
			// synced lyrics. Reporting it would flag a great many normal sidecars.
			name:        "all plain text",
			text:        "just some lyrics\nwith no timestamps at all\n",
			wantLines:   0,
			wantDropped: 2,
			wantPartial: false,
		},
		{
			// Recognized LRC structure is not "dropped".
			name:        "metadata tags are structure",
			text:        "[ar:Artist]\n[ti:Title]\n[offset:+500]\n\n[00:01.00]line\n",
			wantLines:   1,
			wantDropped: 0,
			wantPartial: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lines, dropped := ParseLRC(c.text)
			if len(lines) != c.wantLines {
				t.Errorf("lines = %d, want %d (%+v)", len(lines), c.wantLines, lines)
			}
			if len(dropped) != c.wantDropped {
				t.Errorf("dropped = %d, want %d (%v)", len(dropped), c.wantDropped, dropped)
			}
			if got := LRCPartial(lines, dropped); got != c.wantPartial {
				t.Errorf("LRCPartial = %v, want %v", got, c.wantPartial)
			}
		})
	}
}

// TestWriteWarningsCapsMessage pins the bound at the seam that actually needs it.
// A tag_write_lost diagnostic's detail is this Message verbatim, and the writers
// that persist it (organize, the ReplayGain pass) do not cap it themselves, so this
// is the one place that can. It is also the case capDetail's own doc names as its
// reason to exist. Upstream sanitizes the message but does not bound its length.
func TestWriteWarningsCapsMessage(t *testing.T) {
	huge := strings.Repeat("A", 1<<20)
	ws := []waxlabel.Warning{{
		Code:    waxlabel.WarnValueDropped,
		Message: "cannot store " + huge,
		Keys:    []tag.Key{tag.TrackNumber},
	}}
	got := writeWarnings(ws)
	if len(got) != 1 {
		t.Fatalf("warnings = %d, want 1", len(got))
	}
	if len(got[0].Message) > maxDetailBytes {
		t.Errorf("Message is %d bytes, want <= %d: an unbounded file-derived snippet reaches the catalog",
			len(got[0].Message), maxDetailBytes)
	}
	if !got[0].Unrepresented {
		t.Error("value-dropped must be classified unrepresented")
	}
}

// TestWriteWarningsFansOutKeysAndSurvivesKeyless covers the two shapes the warning
// vocabulary allows: a multi-key warning becomes one entry per key, and a keyless
// one is carried rather than indexed into (Keys is documented as empty for warnings
// that name no key, so w.Keys[0] would panic).
func TestWriteWarningsFansOutKeysAndSurvivesKeyless(t *testing.T) {
	ws := []waxlabel.Warning{
		{Code: waxlabel.WarnValueDropped, Message: "two keys", Keys: []tag.Key{tag.TrackNumber, tag.DiscNumber}},
		{Code: waxlabel.WarnValueCoerced, Message: "no key at all"},
	}
	got := writeWarnings(ws)
	if len(got) != 3 {
		t.Fatalf("entries = %d, want 3 (2 fanned out + 1 keyless)", len(got))
	}
	if got[0].Key != string(tag.TrackNumber) || got[1].Key != string(tag.DiscNumber) {
		t.Errorf("fan-out keys = %q/%q", got[0].Key, got[1].Key)
	}
	if got[2].Key != "" || !got[2].Unrepresented {
		t.Errorf("keyless entry = %+v, want an empty key and still unrepresented", got[2])
	}
}
