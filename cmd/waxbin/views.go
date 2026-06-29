package main

import "github.com/colespringer/waxbin/model"

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
