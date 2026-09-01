package main

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/store/sqlite"
)

// JSON-facing projections of domain types. The CLI owns presentation; it never
// leaks internal ids or raw path bytes.

// printItemTable renders the shared item table for query, query --page, and browse.
// PUBLISHED appears only when the rows hold an episode, since an episode's year is a
// rounded pub date and a whole season otherwise reads as one repeated year; a
// music-only catalog would just get a blank column. A zero TRK or YEAR prints blank.
func printItemTable(w io.Writer, items []*model.ItemView) error {
	// Buffered so each line can be right-trimmed: tabwriter pads empty tab-terminated
	// cells, so a book's blank TRK/YEAR would trail spaces. Trimming afterwards rather
	// than dropping the cells keeps every row's cell count equal, which is what keeps
	// the table one column block.
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 0, 2, 2, ' ', 0)
	withPub := false
	for _, v := range items {
		if v.Kind == model.KindEpisode {
			withPub = true
			break
		}
	}
	header := "PID\tTITLE\tARTIST\tALBUM\tTRK\tYEAR"
	if withPub {
		header += "\tPUBLISHED"
	}
	fmt.Fprintln(tw, header)
	for _, v := range items {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s",
			v.PID, v.Title, v.Artist, v.Album, blankZero(v.TrackNo), blankZero(v.Year))
		if withPub {
			fmt.Fprintf(tw, "\t%s", pubDate(v.PubDateNS))
		}
		fmt.Fprintln(tw)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, line := range strings.SplitAfter(buf.String(), "\n") {
		if line == "" {
			continue
		}
		if _, err := io.WriteString(w, strings.TrimRight(line, " \t\n")+"\n"); err != nil {
			return err
		}
	}
	return nil
}

// blankZero renders a count-like column, with 0 as blank (see printItemTable).
func blankZero(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// pubDate renders a publication time as a date, blank when undated. UTC, not local:
// the stored value is a resolved instant, and the reader's zone would shift the date
// across a day boundary for everyone west of the publisher.
func pubDate(ns int64) string {
	if ns == 0 {
		return ""
	}
	return time.Unix(0, ns).UTC().Format("2006-01-02")
}

type libView struct {
	PID     string `json:"pid"`
	Root    string `json:"root"`
	Mode    string `json:"mode"`
	Media   string `json:"media,omitempty"`
	Profile string `json:"profile"`
}

func libViews(libs []*model.Library) []libView {
	out := make([]libView, 0, len(libs))
	for _, l := range libs {
		out = append(out, libView{
			PID: string(l.PID), Root: l.DisplayRoot, Mode: string(l.Mode),
			Media: string(l.MediaType()), Profile: l.Profile,
		})
	}
	return out
}

type itemView struct {
	PID         string `json:"pid"`
	Kind        string `json:"kind"`
	State       string `json:"state"`
	Title       string `json:"title"`
	Artist      string `json:"artist,omitempty"`
	AlbumArtist string `json:"albumArtist,omitempty"`
	Album       string `json:"album,omitempty"`
	Track       int    `json:"track,omitempty"`
	Disc        int    `json:"disc,omitempty"`
	Year        int    `json:"year,omitempty"`
	Genre       string `json:"genre,omitempty"`
	// Entity handles: the effective artist/album-artist/album/release-group/podcast
	// entity pids, so a consumer can group by real identity. albumPid and
	// releaseGroupPid are absent for a book or episode, releaseGroupPid is also absent
	// for a track whose album has no release group, and podcastPid is present only for
	// an episode.
	ArtistPID       string `json:"artistPid,omitempty"`
	AlbumArtistPID  string `json:"albumArtistPid,omitempty"`
	AlbumPID        string `json:"albumPid,omitempty"`
	ReleaseGroupPID string `json:"releaseGroupPid,omitempty"`
	PodcastPID      string `json:"podcastPid,omitempty"`
	// The registered root the item's primary file sits under, absent for a fileless
	// item such as an undownloaded episode.
	LibraryPID string `json:"libraryPid,omitempty"`
	// External identifiers (see model.ItemView). Only a tagged file or a completed
	// enrich pass supplies one, so an untagged library emits none of them.
	MBID             string `json:"mbid,omitempty"`
	ISRC             string `json:"isrc,omitempty"`
	AlbumMBID        string `json:"albumMbid,omitempty"`
	ReleaseGroupMBID string `json:"releaseGroupMbid,omitempty"`
	ArtistMBID       string `json:"artistMbid,omitempty"`
	AlbumArtistMBID  string `json:"albumArtistMbid,omitempty"`
	// Composer and its collation key, present for track items.
	Composer     string `json:"composer,omitempty"`
	ComposerSort string `json:"composerSort,omitempty"`
	// The track's stated tempo, a whole number, absent when the file states none.
	BPM int `json:"bpm,omitempty"`
	// Episode fields, present for episode items only. pubDateNs is nanoseconds, the
	// unit every timestamp in this JSON contract uses. explicit is the episode's own
	// advisory flag and podcastExplicit its show's; the two are independent, so a
	// restricted consumer checks both.
	Explicit        bool  `json:"explicit,omitempty"`
	PodcastExplicit bool  `json:"podcastExplicit,omitempty"`
	Season          int   `json:"season,omitempty"`
	PubDateNS       int64 `json:"pubDateNs,omitempty"`

	// Everything below is kind-agnostic; the episode block above ends at pubDateNs.
	Source     string `json:"source,omitempty"`
	DurationMS int64  `json:"durationMs,omitempty"`
	Codec      string `json:"codec,omitempty"`
	Path       string `json:"path,omitempty"`
	FilePID    string `json:"filePid,omitempty"`
	// A virtual track (a cue TRACK of a single-file rip) plays only one window within
	// the shared file, and virtual marks it. The window arrives in both coordinate
	// systems: startFrames/endFrames are CD frames (75/sec), the stored truth a
	// consumer converts to an exact sample, and startMs/endMs are the derived
	// milliseconds a player seeks to.
	//
	// These are omitempty like the rest of this view, so a zero bound is absent rather
	// than 0: the leading track of a rip carries no startFrames, and the final one
	// carries no endFrames. Default a missing bound to 0 and each field's contract
	// reads the same either way, since a missing start is the head of the file and a
	// missing end runs to the end of it. Both tracks are complete descriptions rather
	// than truncated ones.
	//
	// Branch on virtual, never on whether a bound is present. virtual is itself
	// omitempty, so a whole-file item omits it along with all four bounds; that
	// absence, not the missing offsets, is what says "no window".
	Virtual     bool  `json:"virtual,omitempty"`
	StartFrames int64 `json:"startFrames,omitempty"`
	EndFrames   int64 `json:"endFrames,omitempty"`
	StartMS     int64 `json:"startMs,omitempty"`
	EndMS       int64 `json:"endMs,omitempty"`
}

func toItemView(v *model.ItemView) itemView {
	return itemView{
		PID: string(v.PID), Kind: string(v.Kind), State: string(v.State), Title: v.Title,
		Artist: v.Artist, AlbumArtist: v.AlbumArtist, Album: v.Album, Track: v.TrackNo,
		Disc: v.DiscNo, Year: v.Year, Genre: v.Genre,
		ArtistPID: string(v.ArtistPID), AlbumArtistPID: string(v.AlbumArtistPID),
		AlbumPID: string(v.AlbumPID), ReleaseGroupPID: string(v.ReleaseGroupPID),
		PodcastPID: string(v.PodcastPID), LibraryPID: string(v.LibraryPID),
		MBID: v.MBID, ISRC: v.ISRC, AlbumMBID: v.AlbumMBID,
		ReleaseGroupMBID: v.ReleaseGroupMBID,
		ArtistMBID:       v.ArtistMBID, AlbumArtistMBID: v.AlbumArtistMBID,
		Composer: v.Composer, ComposerSort: v.ComposerSort, BPM: v.BPM,
		Explicit: v.Explicit, PodcastExplicit: v.PodcastExplicit,
		Season: v.Season, PubDateNS: v.PubDateNS, Source: string(v.Source),
		DurationMS: v.DurationMS, Codec: v.Codec, Path: v.DisplayPath, FilePID: string(v.FilePID),
		Virtual: v.Virtual, StartFrames: v.StartFrames, EndFrames: v.EndFrames,
		StartMS: v.StartMS, EndMS: v.EndMS,
	}
}

func itemViews(items []*model.ItemView) []itemView {
	out := make([]itemView, 0, len(items))
	for _, v := range items {
		out = append(out, toItemView(v))
	}
	return out
}

// loudnessView is the JSON shape for an item's ReplayGain, omitted when absent.
type loudnessJSON struct {
	IntegratedLUFS float64  `json:"integratedLufs"`
	TrackGainDB    float64  `json:"trackGainDb"`
	TrackPeak      float64  `json:"trackPeak"`
	AlbumGainDB    *float64 `json:"albumGainDb,omitempty"`
	AlbumPeak      *float64 `json:"albumPeak,omitempty"`
}

func loudnessView(l *model.Loudness) *loudnessJSON {
	if l == nil {
		return nil
	}
	v := &loudnessJSON{IntegratedLUFS: l.IntegratedLUFS, TrackGainDB: l.TrackGainDB, TrackPeak: l.TrackPeak}
	if l.HasAlbum {
		v.AlbumGainDB, v.AlbumPeak = &l.AlbumGainDB, &l.AlbumPeak
	}
	return v
}

// showView is the JSON shape for `show`: the item plus optional ReplayGain.
type showView struct {
	Item       itemView      `json:"item"`
	ReplayGain *loudnessJSON `json:"replayGain,omitempty"`
	Credits    []creditView  `json:"credits,omitempty"`
}

// chapterView is the JSON shape for one book chapter, with book-timeline offsets.
type chapterView struct {
	Position int    `json:"position"`
	Title    string `json:"title"`
	StartMS  int64  `json:"startMs"`
	EndMS    int64  `json:"endMs"`
	FilePID  string `json:"filePid,omitempty"`
}

func chapterViews(chs []model.Chapter) []chapterView {
	out := make([]chapterView, 0, len(chs))
	for _, c := range chs {
		out = append(out, chapterView{
			Position: c.Position, Title: c.Title, StartMS: c.StartMS, EndMS: c.EndMS,
			FilePID: string(c.FilePID),
		})
	}
	return out
}

// bookPartView is the JSON shape for one backing file of a (possibly multi-file) book.
type bookPartView struct {
	FilePID    string `json:"filePid"`
	Path       string `json:"path"`
	Position   int    `json:"position"`
	DurationMS int64  `json:"durationMs"`
}

// bookView is the JSON shape for `book`: the full audiobook detail.
type bookView struct {
	PID             string         `json:"pid"`
	Title           string         `json:"title"`
	Subtitle        string         `json:"subtitle,omitempty"`
	Authors         []string       `json:"authors,omitempty"`
	AuthorSort      string         `json:"authorSort,omitempty"`
	Narrators       []string       `json:"narrators,omitempty"`
	Translators     []string       `json:"translators,omitempty"`
	Editors         []string       `json:"editors,omitempty"`
	Series          string         `json:"series,omitempty"`
	SeriesPID       string         `json:"seriesPid,omitempty"`
	SeriesSeq       string         `json:"seriesSeq,omitempty"`
	Year            int            `json:"year,omitempty"`
	Publisher       string         `json:"publisher,omitempty"`
	ASIN            string         `json:"asin,omitempty"`
	ISBN            string         `json:"isbn,omitempty"`
	Edition         string         `json:"edition,omitempty"`
	Abridged        *bool          `json:"abridged,omitempty"`
	Description     string         `json:"description,omitempty"`
	TotalDurationMS int64          `json:"totalDurationMs"`
	Parts           []bookPartView `json:"parts"`
	Chapters        []chapterView  `json:"chapters"`
}

func toBookView(d *model.BookDetail) bookView {
	v := bookView{
		PID: string(d.Item.PID), Title: d.Item.Title, Subtitle: d.Subtitle,
		Authors: d.Authors, AuthorSort: d.Item.AuthorSort,
		Narrators: d.Narrators, Translators: d.Translators, Editors: d.Editors,
		Series: d.Series, SeriesPID: string(d.SeriesPID), SeriesSeq: d.SeriesSeq,
		Year: d.Item.Year, Publisher: d.Publisher, ASIN: d.ASIN, ISBN: d.ISBN, Edition: d.Edition,
		Abridged: d.Abridged, Description: d.Description, TotalDurationMS: d.TotalDurationMS,
		Chapters: chapterViews(d.Chapters),
	}
	for _, p := range d.Files {
		v.Parts = append(v.Parts, bookPartView{
			FilePID: string(p.FilePID), Path: p.DisplayPath, Position: p.Position, DurationMS: p.DurationMS,
		})
	}
	return v
}

type userView struct {
	PID       string `json:"pid"`
	Name      string `json:"name"`
	IsDefault bool   `json:"isDefault"`
}

func userViews(users []*model.User) []userView {
	out := make([]userView, 0, len(users))
	for _, u := range users {
		out = append(out, userView{PID: string(u.PID), Name: u.Name, IsDefault: u.IsDefault})
	}
	return out
}

type playStateView struct {
	ItemPID    string `json:"itemPid"`
	PositionMS int64  `json:"positionMs"`
	Played     bool   `json:"played"`
	Finished   bool   `json:"finished"`
	PlayCount  int    `json:"playCount"`
	Rating     *int   `json:"rating,omitempty"`
	Starred    bool   `json:"starred"`
	// The stamps are unix-ns epochs, JSON-encoded as decimal strings (",string")
	// like every ns timestamp in the CLI JSON contract: the values exceed
	// IEEE-754 double precision, so a bare number would be corrupted by any
	// loose-JSON consumer. 0 (never set) is omitted.
	//
	// lastPlayedAt moves on a play and lastProgressAt on a play or a checkpoint, so
	// a checkpointed-but-unplayed item carries the second and not the first. Neither
	// moves for a star or a rating.
	LastPlayedAt     int64 `json:"lastPlayedAt,string,omitempty"`
	LastProgressAt   int64 `json:"lastProgressAt,string,omitempty"`
	RatingChangedAt  int64 `json:"ratingChangedAt,string,omitempty"`
	StarredChangedAt int64 `json:"starredChangedAt,string,omitempty"`
	PlayedChangedAt  int64 `json:"playedChangedAt,string,omitempty"`
}

func toPlayStateView(st *model.PlayState) playStateView {
	v := playStateView{
		ItemPID: string(st.ItemPID), PositionMS: st.PositionMS, Played: st.Played,
		Finished: st.Finished, PlayCount: st.PlayCount, Starred: st.Starred,
		LastPlayedAt: st.LastPlayedAt, LastProgressAt: st.LastProgressAt,
		RatingChangedAt: st.RatingChangedAt, StarredChangedAt: st.StarredChangedAt,
		PlayedChangedAt: st.PlayedChangedAt,
	}
	if st.HasRating {
		r := st.Rating
		v.Rating = &r
	}
	return v
}

type bucketView struct {
	Key       string `json:"key,omitempty"`
	Display   string `json:"display"`
	Count     int    `json:"count"`
	Unknown   bool   `json:"unknown,omitempty"`
	EntityPID string `json:"entityPid,omitempty"`
}

type facetView struct {
	GroupBy string       `json:"groupBy"`
	Buckets []bucketView `json:"buckets"`
}

func toFacetView(r *read.FacetResult) facetView {
	return facetView{GroupBy: string(r.GroupBy), Buckets: bucketViews(r.Buckets)}
}

// pageView is the JSON shape for a keyset-paginated item window.
type pageView struct {
	Items      []itemView `json:"items"`
	NextCursor string     `json:"nextCursor,omitempty"`
	HasMore    bool       `json:"hasMore"`
}

func toPageView(p *read.Page) pageView {
	return pageView{Items: itemViews(p.Items), NextCursor: string(p.Next), HasMore: p.HasMore}
}

type statsView struct {
	Items         int           `json:"items"`
	Books         int           `json:"books"`
	Artists       int           `json:"artists"`
	ReleaseGroups int           `json:"releaseGroups"`
	Albums        int           `json:"albums"`
	Genres        int           `json:"genres"`
	TotalDuration int64         `json:"totalDurationMs"`
	TopGenres     []bucketView  `json:"topGenres"`
	TopArtists    []bucketView  `json:"topArtists"`
	ByYear        []bucketView  `json:"byYear"`
	Play          playStatsJSON `json:"play"`
}

type playStatsJSON struct {
	User       string           `json:"user"`
	TotalPlays int              `json:"totalPlays"`
	Finished   int              `json:"finished"`
	Starred    int              `json:"starred"`
	MostPlayed []playedItemJSON `json:"mostPlayed"`
}

type playedItemJSON struct {
	PID       string `json:"pid"`
	Title     string `json:"title"`
	Artist    string `json:"artist,omitempty"`
	PlayCount int    `json:"playCount"`
}

func toStatsView(s *read.Stats) statsView {
	v := statsView{
		Items: s.Items, Books: s.Books, Artists: s.Artists, ReleaseGroups: s.ReleaseGroups, Albums: s.Albums,
		Genres: s.Genres, TotalDuration: s.TotalDuration,
		TopGenres: bucketViews(s.TopGenres), TopArtists: bucketViews(s.TopArtists), ByYear: bucketViews(s.ByYear),
		Play: playStatsJSON{
			User: s.Play.User, TotalPlays: s.Play.TotalPlays, Finished: s.Play.Finished, Starred: s.Play.Starred,
		},
	}
	for _, p := range s.Play.MostPlayed {
		v.Play.MostPlayed = append(v.Play.MostPlayed, playedItemJSON{
			PID: string(p.PID), Title: p.Title, Artist: p.Artist, PlayCount: p.PlayCount,
		})
	}
	return v
}

// yearReviewView is the camelCase JSON shape for a listening year-in-review, so
// `stats --year --json` matches the stable schema of every other --json path
// instead of leaking the untagged read.YearReview/read.Bucket fields.
type yearReviewView struct {
	Year          int              `json:"year"`
	User          string           `json:"user"`
	Sessions      int              `json:"sessions"`
	MinutesPlayed int64            `json:"minutesPlayed"`
	TracksPlayed  int              `json:"tracksPlayed"`
	NewInLibrary  int              `json:"newInLibrary"`
	TopArtists    []bucketView     `json:"topArtists"`
	TopGenres     []bucketView     `json:"topGenres"`
	TopTracks     []playedItemJSON `json:"topTracks"`
}

func toYearReviewView(yr *read.YearReview) yearReviewView {
	v := yearReviewView{
		Year: yr.Year, User: yr.User, Sessions: yr.Sessions, MinutesPlayed: yr.MinutesPlayed,
		TracksPlayed: yr.TracksPlayed, NewInLibrary: yr.NewInLibrary,
		TopArtists: bucketViews(yr.TopArtists), TopGenres: bucketViews(yr.TopGenres),
	}
	for _, p := range yr.TopTracks {
		v.TopTracks = append(v.TopTracks, playedItemJSON{
			PID: string(p.PID), Title: p.Title, Artist: p.Artist, PlayCount: p.PlayCount,
		})
	}
	return v
}

// playlistView is the JSON shape for a playlist's metadata.
type playlistView struct {
	PID        string `json:"pid"`
	Name       string `json:"name"`
	Owner      string `json:"owner"`
	Kind       string `json:"kind"`
	Visibility string `json:"visibility"`
	ItemCount  int    `json:"itemCount"`
	HasArt     bool   `json:"hasArt"`
}

// toPlaylistView takes the count rather than reading model.Playlist.ItemCount, which
// is always 0 for a smart playlist because its membership is computed on read. The
// caller has already resolved the real number for its text output, so passing it in is
// what keeps --json from reporting 0 where the table reports 42.
func toPlaylistView(p *model.Playlist, count int) playlistView {
	return playlistView{
		PID: string(p.PID), Name: p.Name, Owner: p.OwnerName, Kind: string(p.Kind),
		Visibility: string(p.Visibility), ItemCount: count, HasArt: p.HasArt,
	}
}

func playlistViews(pls []*model.Playlist, counts map[model.PID]int) []playlistView {
	out := make([]playlistView, 0, len(pls))
	for _, p := range pls {
		out = append(out, toPlaylistView(p, counts[p.PID]))
	}
	return out
}

// lyricsView is the JSON shape for an item's structured lyrics.
type lyricsView struct {
	Source   string           `json:"source"`
	Provider string           `json:"provider,omitempty"`
	Updated  int64            `json:"updatedAt,string"` // unix ns; a string, see playStateView
	Synced   bool             `json:"synced"`
	Lines    []syncedLineView `json:"lines,omitempty"`
	Unsynced string           `json:"unsynced,omitempty"`
}

type syncedLineView struct {
	MS   int64  `json:"ms"`
	Text string `json:"text"`
}

func toLyricsView(ly *model.Lyrics) lyricsView {
	v := lyricsView{Source: string(ly.Source), Provider: ly.Provider, Updated: ly.UpdatedAt,
		Synced: len(ly.Synced) > 0, Unsynced: ly.Unsynced}
	for _, l := range ly.Synced {
		v.Lines = append(v.Lines, syncedLineView{MS: l.TimeMS, Text: l.Text})
	}
	return v
}

// searchHitView is the JSON shape for one ranked search hit.
type searchHitView struct {
	PID      string  `json:"pid"`
	Kind     string  `json:"kind"`
	Title    string  `json:"title"`
	Subtitle string  `json:"subtitle,omitempty"`
	Score    float64 `json:"score"`
}

type searchView struct {
	Query     string          `json:"query"`
	Artists   []searchHitView `json:"artists"`
	Albums    []searchHitView `json:"albums"`
	Tracks    []searchHitView `json:"tracks"`
	Books     []searchHitView `json:"books"`
	Episodes  []searchHitView `json:"episodes"`
	Truncated bool            `json:"truncated,omitempty"`
}

func toSearchView(r *read.SearchResult) searchView {
	return searchView{
		Query:   r.Query,
		Artists: hitViews(r.Artists), Albums: hitViews(r.Albums),
		Tracks: hitViews(r.Tracks), Books: hitViews(r.Books),
		Episodes: hitViews(r.Episodes), Truncated: r.Truncated,
	}
}

func hitViews(hits []read.SearchHit) []searchHitView {
	out := make([]searchHitView, 0, len(hits))
	for _, h := range hits {
		out = append(out, searchHitView{
			PID: string(h.PID), Kind: h.Kind, Title: h.Title, Subtitle: h.Subtitle, Score: h.Score,
		})
	}
	return out
}

func bucketViews(buckets []read.Bucket) []bucketView {
	out := make([]bucketView, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, bucketView{
			Key: b.Key, Display: b.Display, Count: b.Count, Unknown: b.IsUnknown, EntityPID: string(b.EntityPID),
		})
	}
	return out
}

type jobView struct {
	PID      string  `json:"pid"`
	Kind     string  `json:"kind"`
	Scope    string  `json:"scope"`
	State    string  `json:"state"`
	Owner    string  `json:"owner"`
	Progress float64 `json:"progress"`
	Message  string  `json:"message,omitempty"`
	Error    string  `json:"error,omitempty"`
}

type derivedView struct {
	ItemsMissingFTS         int `json:"itemsMissingFts"`
	OrphanFTSRows           int `json:"orphanFtsRows"`
	ArtistRollupDrift       int `json:"artistRollupDrift"`
	GenreRollupDrift        int `json:"genreRollupDrift"`
	ReleaseGroupRollupDrift int `json:"releaseGroupRollupDrift"`
	SortKeyDrift            int `json:"sortKeyDrift"`
	BookDurationDrift       int `json:"bookDurationDrift"`
	BookISBNKeyDrift        int `json:"bookIsbnKeyDrift"`
	OrphanArtSources        int `json:"orphanArtSources"`
	OrphanThumbnails        int `json:"orphanThumbnails"`
	// Custom-tag provenance rows under a key WaxBin has since reserved.
	OrphanReservedTagProvenance int `json:"orphanReservedTagProvenance"`
	// Custom-tag rows and locks under a key the key rule no longer accepts.
	StrandedTagKeyRows int  `json:"strandedTagKeyRows"`
	Consistent         bool `json:"consistent"`
	// Present only when --fix rewrote sort keys, which invalidates open page cursors.
	SortKeysRewritten int `json:"sortKeysRewritten,omitempty"`
}

func toDerivedView(r *sqlite.DerivedReport) derivedView {
	return derivedView{
		ItemsMissingFTS: r.ItemsMissingFTS, OrphanFTSRows: r.OrphanFTSRows,
		ArtistRollupDrift: r.ArtistRollupDrift, GenreRollupDrift: r.GenreRollupDrift,
		ReleaseGroupRollupDrift: r.ReleaseGroupRollupDrift, SortKeyDrift: r.SortKeyDrift,
		BookDurationDrift: r.BookDurationDrift, BookISBNKeyDrift: r.BookISBNKeyDrift,
		OrphanArtSources: r.OrphanArtSources, OrphanThumbnails: r.OrphanThumbnails,
		OrphanReservedTagProvenance: r.OrphanReservedTagProvenance,
		StrandedTagKeyRows:          r.StrandedTagKeyRows,
		Consistent:                  r.Consistent(),
	}
}

type analyzeView struct {
	Analyzed         int `json:"analyzed"`
	LoudnessMeasured int `json:"loudnessMeasured"`
	// The three rg-tag counters are omitempty as a set: a write-back pass that was
	// never asked for reports none of them, while a pass that ran reports whichever
	// are non-zero.
	ReplayGainTagsWritten       int    `json:"replayGainTagsWritten,omitempty"`
	ReplayGainTagsFailed        int    `json:"replayGainTagsFailed,omitempty"`
	ReplayGainTagsUnrepresented int    `json:"replayGainTagsUnrepresented,omitempty"`
	Skipped                     int    `json:"skipped"`
	Errored                     int    `json:"errored"`
	JobPID                      string `json:"jobPid,omitempty"`
}

func toAnalyzeView(r *waxbin.AnalyzeResult) analyzeView {
	return analyzeView{
		Analyzed: r.Result.Analyzed, LoudnessMeasured: r.Result.LoudnessMeasured,
		ReplayGainTagsWritten:       r.Result.ReplayGainTagsWritten,
		ReplayGainTagsFailed:        r.Result.ReplayGainTagsFailed,
		ReplayGainTagsUnrepresented: r.Result.ReplayGainTagsUnrepresented,
		Skipped:                     r.Result.Skipped, Errored: r.Result.Errored, JobPID: string(r.JobPID),
	}
}

type enrichView struct {
	ArtistsEnriched       int `json:"artistsEnriched"`
	ArtistsMatched        int `json:"artistsMatched"`
	ReleaseGroupsEnriched int `json:"releaseGroupsEnriched"`
	ReleaseGroupsMatched  int `json:"releaseGroupsMatched"`
	AlbumsSearched        int `json:"albumsSearched"`
	AlbumsMatched         int `json:"albumsMatched"`
	BooksEnriched         int `json:"booksEnriched"`
	BooksMatched          int `json:"booksMatched"`
	LyricsEnriched        int `json:"lyricsEnriched"`
	LyricsMatched         int `json:"lyricsMatched"`
	// Both art backfill phases are gated on an injected provider, so their counts are
	// omitted when they did not run and the stock payload keeps the shape it had.
	AuxArtEnriched    int    `json:"auxArtEnriched,omitempty"`
	AuxArtMatched     int    `json:"auxArtMatched,omitempty"`
	ArtistArtEnriched int    `json:"artistArtEnriched,omitempty"`
	ArtistArtMatched  int    `json:"artistArtMatched,omitempty"`
	ArtFetched        int    `json:"artFetched"`
	AuxArtFetched     int    `json:"auxArtFetched,omitempty"`
	TagsWritten       int    `json:"tagsWritten,omitempty"`
	TagsFailed        int    `json:"tagsFailed,omitempty"`
	TagsUnrepresented int    `json:"tagsUnrepresented,omitempty"`
	TagsSkipped       int    `json:"tagsSkipped,omitempty"`
	JobPID            string `json:"jobPid,omitempty"`
}

func toEnrichView(r *waxbin.EnrichResult) enrichView {
	return enrichView{
		ArtistsEnriched: r.Result.ArtistsEnriched, ArtistsMatched: r.Result.ArtistsMatched,
		ReleaseGroupsEnriched: r.Result.ReleaseGroupsEnriched, ReleaseGroupsMatched: r.Result.ReleaseGroupsMatched,
		AlbumsSearched: r.Result.AlbumsSearched, AlbumsMatched: r.Result.AlbumsMatched,
		BooksEnriched: r.Result.BooksEnriched, BooksMatched: r.Result.BooksMatched,
		LyricsEnriched: r.Result.LyricsEnriched, LyricsMatched: r.Result.LyricsMatched,
		AuxArtEnriched: r.Result.AuxArtEnriched, AuxArtMatched: r.Result.AuxArtMatched,
		ArtistArtEnriched: r.Result.ArtistArtEnriched, ArtistArtMatched: r.Result.ArtistArtMatched,
		ArtFetched: r.Result.ArtFetched, AuxArtFetched: r.Result.AuxArtFetched,
		TagsWritten: r.Result.TagsWritten, TagsFailed: r.Result.TagsFailed,
		TagsUnrepresented: r.Result.TagsUnrepresented, TagsSkipped: r.Result.TagsSkipped,
		JobPID: string(r.JobPID),
	}
}

func jobViews(jobs []*model.Job) []jobView {
	out := make([]jobView, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, jobView{
			PID: string(j.PID), Kind: j.Kind, Scope: j.Scope, State: string(j.State),
			Owner: j.Owner, Progress: j.Progress, Message: j.Message, Error: j.Error,
		})
	}
	return out
}

// attribution renders a provenance source for a human: the source alone, or the
// source with the provider that supplied it and the URL it came from. It is shared by
// the provenance table and the lyrics header so the two cannot drift.
func attribution(source model.ProvenanceSource, provider, sourceURL string) string {
	s := string(source)
	if provider != "" {
		s += " (" + provider + ")"
	}
	if sourceURL != "" {
		s += " " + sourceURL
	}
	return s
}
