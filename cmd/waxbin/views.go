package main

import (
	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/store/sqlite"
)

// JSON-facing projections of domain types. The CLI owns presentation; it never
// leaks internal ids or raw path bytes.

type libView struct {
	PID     string `json:"pid"`
	Root    string `json:"root"`
	Mode    string `json:"mode"`
	Profile string `json:"profile"`
}

func libViews(libs []*model.Library) []libView {
	out := make([]libView, 0, len(libs))
	for _, l := range libs {
		out = append(out, libView{PID: string(l.PID), Root: l.DisplayRoot, Mode: string(l.Mode), Profile: l.Profile})
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
	DurationMS  int64  `json:"durationMs,omitempty"`
	Codec       string `json:"codec,omitempty"`
	Path        string `json:"path,omitempty"`
	FilePID     string `json:"filePid,omitempty"`
}

func toItemView(v *model.ItemView) itemView {
	return itemView{
		PID: string(v.PID), Kind: string(v.Kind), State: string(v.State), Title: v.Title,
		Artist: v.Artist, AlbumArtist: v.AlbumArtist, Album: v.Album, Track: v.TrackNo,
		Disc: v.DiscNo, Year: v.Year, Genre: v.Genre, DurationMS: v.DurationMS,
		Codec: v.Codec, Path: v.DisplayPath, FilePID: string(v.FilePID),
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
}

func toPlayStateView(st *model.PlayState) playStateView {
	v := playStateView{
		ItemPID: string(st.ItemPID), PositionMS: st.PositionMS, Played: st.Played,
		Finished: st.Finished, PlayCount: st.PlayCount, Starred: st.Starred,
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
		Items: s.Items, Artists: s.Artists, ReleaseGroups: s.ReleaseGroups, Albums: s.Albums,
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

// playlistView is the JSON shape for a playlist's metadata.
type playlistView struct {
	PID        string `json:"pid"`
	Name       string `json:"name"`
	Owner      string `json:"owner"`
	Kind       string `json:"kind"`
	Visibility string `json:"visibility"`
	ItemCount  int    `json:"itemCount"`
}

func toPlaylistView(p *model.Playlist) playlistView {
	return playlistView{
		PID: string(p.PID), Name: p.Name, Owner: p.OwnerName, Kind: string(p.Kind),
		Visibility: string(p.Visibility), ItemCount: p.ItemCount,
	}
}

func playlistViews(pls []*model.Playlist) []playlistView {
	out := make([]playlistView, 0, len(pls))
	for _, p := range pls {
		out = append(out, toPlaylistView(p))
	}
	return out
}

// lyricsView is the JSON shape for an item's structured lyrics.
type lyricsView struct {
	Source   string           `json:"source"`
	Synced   bool             `json:"synced"`
	Lines    []syncedLineView `json:"lines,omitempty"`
	Unsynced string           `json:"unsynced,omitempty"`
}

type syncedLineView struct {
	MS   int64  `json:"ms"`
	Text string `json:"text"`
}

func toLyricsView(ly *model.Lyrics) lyricsView {
	v := lyricsView{Source: ly.Source, Synced: len(ly.Synced) > 0, Unsynced: ly.Unsynced}
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
	Episodes  []searchHitView `json:"episodes"`
	Truncated bool            `json:"truncated,omitempty"`
}

func toSearchView(r *read.SearchResult) searchView {
	return searchView{
		Query:   r.Query,
		Artists: hitViews(r.Artists), Albums: hitViews(r.Albums),
		Tracks: hitViews(r.Tracks), Episodes: hitViews(r.Episodes), Truncated: r.Truncated,
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
	ItemsMissingFTS         int  `json:"itemsMissingFts"`
	OrphanFTSRows           int  `json:"orphanFtsRows"`
	ArtistRollupDrift       int  `json:"artistRollupDrift"`
	GenreRollupDrift        int  `json:"genreRollupDrift"`
	ReleaseGroupRollupDrift int  `json:"releaseGroupRollupDrift"`
	SortKeyDrift            int  `json:"sortKeyDrift"`
	OrphanArtSources        int  `json:"orphanArtSources"`
	OrphanThumbnails        int  `json:"orphanThumbnails"`
	Consistent              bool `json:"consistent"`
}

func toDerivedView(r *sqlite.DerivedReport) derivedView {
	return derivedView{
		ItemsMissingFTS: r.ItemsMissingFTS, OrphanFTSRows: r.OrphanFTSRows,
		ArtistRollupDrift: r.ArtistRollupDrift, GenreRollupDrift: r.GenreRollupDrift,
		ReleaseGroupRollupDrift: r.ReleaseGroupRollupDrift, SortKeyDrift: r.SortKeyDrift,
		OrphanArtSources: r.OrphanArtSources, OrphanThumbnails: r.OrphanThumbnails,
		Consistent: r.Consistent(),
	}
}

type analyzeView struct {
	Analyzed         int    `json:"analyzed"`
	LoudnessMeasured int    `json:"loudnessMeasured"`
	Skipped          int    `json:"skipped"`
	Errored          int    `json:"errored"`
	JobPID           string `json:"jobPid,omitempty"`
}

func toAnalyzeView(r *waxbin.AnalyzeResult) analyzeView {
	return analyzeView{
		Analyzed: r.Result.Analyzed, LoudnessMeasured: r.Result.LoudnessMeasured,
		Skipped: r.Result.Skipped, Errored: r.Result.Errored, JobPID: string(r.JobPID),
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
