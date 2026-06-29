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
	out := facetView{GroupBy: string(r.GroupBy), Buckets: make([]bucketView, 0, len(r.Buckets))}
	for _, b := range r.Buckets {
		out.Buckets = append(out.Buckets, bucketView{
			Key: b.Key, Display: b.Display, Count: b.Count,
			Unknown: b.IsUnknown, EntityPID: string(b.EntityPID),
		})
	}
	return out
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
	Consistent              bool `json:"consistent"`
}

func toDerivedView(r *sqlite.DerivedReport) derivedView {
	return derivedView{
		ItemsMissingFTS: r.ItemsMissingFTS, OrphanFTSRows: r.OrphanFTSRows,
		ArtistRollupDrift: r.ArtistRollupDrift, GenreRollupDrift: r.GenreRollupDrift,
		ReleaseGroupRollupDrift: r.ReleaseGroupRollupDrift, SortKeyDrift: r.SortKeyDrift,
		Consistent: r.Consistent(),
	}
}

type analyzeView struct {
	Analyzed int    `json:"analyzed"`
	Skipped  int    `json:"skipped"`
	Errored  int    `json:"errored"`
	JobPID   string `json:"jobPid,omitempty"`
}

func toAnalyzeView(r *waxbin.AnalyzeResult) analyzeView {
	return analyzeView{
		Analyzed: r.Result.Analyzed, Skipped: r.Result.Skipped,
		Errored: r.Result.Errored, JobPID: string(r.JobPID),
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
