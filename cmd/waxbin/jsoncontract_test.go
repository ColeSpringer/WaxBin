package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/podcast"
	"github.com/colespringer/waxbin/query"
)

// jsonFixture is one catalog holding something for every data command to work on.
type jsonFixture struct {
	db, root, assets            string
	one, two, three, four, book model.PID
	artist, other, album        model.PID
	mix, smart, bob             model.PID
	show, episode, library      model.PID
}

func writeAsset(t *testing.T, path string, b []byte) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func newJSONFixture(t *testing.T) *jsonFixture {
	t.Helper()
	t.Setenv("WAXBIN_CONFIG", "")
	ctx := context.Background()
	fx := &jsonFixture{root: t.TempDir(), assets: t.TempDir(), db: filepath.Join(t.TempDir(), "catalog.db")}
	for i, s := range []struct{ title, artist, album string }{
		{"One", "Artist", "Album"}, {"Two", "Artist", "Album"}, {"Three", "Artist", "Album"}, {"Four", "Other", "Other Album"},
	} {
		writeAsset(t, filepath.Join(fx.root, s.title+".mp3"),
			testaudio.BuildMP3WithAudio(s.title, s.artist, s.album, i+1, testaudio.AudioWithSeed(byte(110+i))))
	}
	writeAsset(t, filepath.Join(fx.root, "book.m4b"),
		testaudio.BuildMP3WithAudio("A Book", "An Author", "", 1, testaudio.AudioWithSeed(120)))
	writeAsset(t, filepath.Join(fx.assets, "staging", "Staged.mp3"),
		testaudio.BuildMP3WithAudio("Staged", "Stager", "Staged Album", 1, testaudio.AudioWithSeed(121)))
	writeAsset(t, filepath.Join(fx.assets, "acquired.mp3"),
		testaudio.BuildMP3WithAudio("Acquired", "Acquirer", "Acquired Album", 1, testaudio.AudioWithSeed(122)))
	var img bytes.Buffer
	pic := image.NewRGBA(image.Rect(0, 0, 4, 4))
	pic.Set(1, 1, color.RGBA{200, 10, 10, 255})
	if err := png.Encode(&img, pic); err != nil {
		t.Fatal(err)
	}
	writeAsset(t, filepath.Join(fx.assets, "cover.png"), img.Bytes())
	writeAsset(t, filepath.Join(fx.assets, "one.lrc"), []byte("[00:01.00]first line\n[00:02.00]second line\n"))
	writeAsset(t, filepath.Join(fx.assets, "chapters.cue"), []byte("FILE \"book.m4b\" MP3\n  TRACK 01 AUDIO\n    TITLE \"Opening\"\n    INDEX 01 00:00:00\n"))
	writeAsset(t, filepath.Join(fx.assets, "show.vtt"), []byte("WEBVTT\n\n00:00.000 --> 00:01.000\nhello\n"))
	writeAsset(t, filepath.Join(fx.assets, "feeds.opml"), []byte(`<?xml version="1.0"?><opml version="2.0"><body>`+
		`<outline type="rss" text="Gone" xmlUrl="https://example.invalid/feed.xml"/></body></opml>`))

	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: fx.db,
		Roots: []config.Root{{Path: fx.root, Mode: model.ModeManaged, Profile: "waxbin-native"}}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	byTitle := map[string]*model.ItemView{}
	for _, it := range items {
		byTitle[it.Title] = it
	}
	for _, title := range []string{"One", "Two", "Three", "Four", "A Book"} {
		if byTitle[title] == nil {
			t.Fatalf("fixture has no %q among %d items", title, len(items))
		}
	}
	fx.one, fx.two, fx.three, fx.four, fx.book = byTitle["One"].PID, byTitle["Two"].PID, byTitle["Three"].PID,
		byTitle["Four"].PID, byTitle["A Book"].PID
	fx.artist, fx.album, fx.other = byTitle["One"].ArtistPID, byTitle["One"].AlbumPID, byTitle["Four"].ArtistPID
	fx.library = byTitle["One"].LibraryPID

	rule := query.New(query.EntityItems).Where("artist", query.OpIs, "Artist").Build()
	doc, err := query.MarshalRule(rule)
	if err != nil {
		t.Fatal(err)
	}
	writeAsset(t, filepath.Join(fx.assets, "rule.json"), doc)
	if fx.smart, err = lib.Playlists().CreateSmart(ctx, "Smart", "", "", rule); err != nil {
		t.Fatalf("smart: %v", err)
	}
	if fx.mix, err = lib.Playlists().CreateStatic(ctx, "Mix", "", ""); err != nil {
		t.Fatalf("mix: %v", err)
	}
	if err := lib.Playlists().Set(ctx, fx.mix, []model.PID{fx.one, fx.two, fx.one}); err != nil {
		t.Fatalf("mix items: %v", err)
	}
	var m3u bytes.Buffer
	if err := lib.Playlists().ExportM3U8(ctx, fx.mix, &m3u, ""); err != nil {
		t.Fatalf("m3u8: %v", err)
	}
	writeAsset(t, filepath.Join(fx.assets, "mix.m3u8"), m3u.Bytes())
	bob, err := lib.CreateUser(ctx, "bob")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	fx.bob = bob.PID
	show, err := lib.Podcasts().AddManual(ctx, "Show", podcast.ManualOptions{})
	if err != nil {
		t.Fatalf("manual show: %v", err)
	}
	fx.show = show.PID
	ep, err := lib.Podcasts().AddEpisode(ctx, fx.show, model.FeedEpisode{Title: "Ep", EnclosureURL: "https://example.invalid/ep.mp3"}, true)
	if err != nil {
		t.Fatalf("episode: %v", err)
	}
	fx.episode = ep.EpisodePID
	writeAsset(t, filepath.Join(fx.assets, "edits.json"), []byte(`[{"itemPid":"`+string(fx.three)+`","fields":{"comment":"batch"}}]`))
	writeAsset(t, filepath.Join(fx.assets, "credits.json"), []byte(`[{"itemPid":"`+string(fx.three)+`","role":"composer","names":["Batch Composer"]}]`))
	if err := lib.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return fx
}

// runJSONCommand runs one command with --json and returns both streams. bare leaves the
// global --root out, for restore, whose own --root shadows it.
func runJSONCommand(fx *jsonFixture, args []string, bare bool) (string, string, error) {
	cmd := newRootCmd(&globals{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	full := []string{"--db", fx.db, "--json"}
	if !bare {
		full = append(full, "--root", fx.root+":managed:waxbin-native")
	}
	cmd.SetArgs(append(full, args...))
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), stderr.String(), err
}

// oneEnvelope reports whether out is exactly one JSON envelope, and why not.
func oneEnvelope(out string) (map[string]json.RawMessage, string) {
	dec := json.NewDecoder(strings.NewReader(out))
	var env map[string]json.RawMessage
	if err := dec.Decode(&env); err != nil {
		return nil, "not JSON: " + err.Error()
	}
	if dec.More() {
		return nil, "more than one JSON value"
	}
	var rest json.RawMessage
	if err := dec.Decode(&rest); err == nil {
		return nil, "trailing output after the envelope"
	}
	for _, k := range []string{"schemaVersion", "command", "data"} {
		if _, ok := env[k]; !ok {
			return nil, "no " + k + " in the envelope"
		}
	}
	return env, ""
}

// TestEveryCommandAnswersJSON runs each data command with --json against one catalog,
// in an order where nothing undoes what a later command needs, and requires its stdout
// to be one JSON envelope when it succeeds, and when it fails either nothing or one
// envelope whose data names the failure (an error member), so no host reads a partial
// run as a success. serve and watch run until stopped, completion prints shell code,
// and help is help.
func TestEveryCommandAnswersJSON(t *testing.T) {
	fx := newJSONFixture(t)
	p := func(pid model.PID) string { return string(pid) }
	asset := func(name string) string { return filepath.Join(fx.assets, name) }
	added := t.TempDir()
	cases := []struct {
		args    []string
		wantErr bool
		bare    bool
	}{
		{args: []string{"version"}},
		{args: []string{"exit-codes"}},
		{args: []string{"profiles"}},
		{args: []string{"doctor"}},
		{args: []string{"jobs"}},
		{args: []string{"library", "list"}},
		{args: []string{"user", "list"}},
		{args: []string{"query"}},
		{args: []string{"search", "One"}},
		{args: []string{"search", "nothing-matches-this"}},
		{args: []string{"browse", "recently-added"}},
		{args: []string{"facet", "--group-by", "genre"}},
		{args: []string{"show", p(fx.one)}},
		{args: []string{"provenance", p(fx.one)}},
		{args: []string{"acquisition", p(fx.one)}},
		{args: []string{"credit", p(fx.one)}},
		{args: []string{"tag", p(fx.one)}},
		{args: []string{"tag", "keys"}},
		{args: []string{"lyrics", p(fx.one)}, wantErr: true},
		{args: []string{"art", "roles", p(fx.one)}},
		{args: []string{"book", p(fx.book)}},
		{args: []string{"chapters", p(fx.book)}},
		{args: []string{"state", "show", p(fx.one)}},
		{args: []string{"stats"}},
		{args: []string{"stats", "--year", "2026"}},
		{args: []string{"entity", "list", "artist"}},
		{args: []string{"entity", "show", "artist", p(fx.artist)}},
		{args: []string{"entity", "info", "artist", p(fx.artist)}},
		{args: []string{"entity", "state", "artist", p(fx.artist)}},
		{args: []string{"entity", "stars", "artist"}},
		{args: []string{"playlist", "list"}},
		{args: []string{"playlist", "show", p(fx.mix)}},
		{args: []string{"playlist", "export", p(fx.mix)}},
		{args: []string{"playlist", "export", p(fx.mix), "--out", asset("out.m3u8")}},
		{args: []string{"playlist", "import", "Preview", "--file", asset("mix.m3u8"), "--dry-run"}},
		{args: []string{"smartplaylist", "export-nsp", p(fx.smart)}},
		{args: []string{"podcast", "list"}},
		{args: []string{"podcast", "show", p(fx.show)}},
		{args: []string{"podcast", "episode", p(fx.episode)}},
		{args: []string{"podcast", "transcript", p(fx.episode)}, wantErr: true},
		{args: []string{"opml", "export"}},
		{args: []string{"opml", "export", asset("out.opml")}},
		{args: []string{"diagnostics", "list"}},
		{args: []string{"diagnostics", "summary"}},
		{args: []string{"audit"}},
		{args: []string{"upgrade"}},
		{args: []string{"manifest"}},
		{args: []string{"export"}},
		{args: []string{"export", asset("export.json")}},
		{args: []string{"db", "verify"}},
		{args: []string{"db", "thumbs"}},
		{args: []string{"db", "enrich-cache"}},
		{args: []string{"inbox", "list"}},
		{args: []string{"inbox", "history"}},
		{args: []string{"trash", "list"}},
		{args: []string{"organize"}},
		{args: []string{"rm", p(fx.two)}},
		{args: []string{"inbox", "import", asset("staging")}},

		{args: []string{"user", "add", "carol"}},
		{args: []string{"state", "set", p(fx.one), "--played"}},
		{args: []string{"entity", "star", "artist", p(fx.artist)}},
		{args: []string{"entity", "rate", "artist", p(fx.artist), "80"}},
		{args: []string{"entity", "edit", "artist", p(fx.artist), "--set", "sort=Artist, The"}},
		{args: []string{"entity", "rename", "album", p(fx.album), "--set", "album=Album Renamed"}},
		{args: []string{"edit", p(fx.one), "--set", "comment=hello"}},
		{args: []string{"edit", "--artist", "Artist", "--set", "genre=Rock", "--dry-run"}},
		{args: []string{"edit", "--artist", "Artist", "--set", "genre=Rock"}},
		{args: []string{"edit", "--artist", "Artist", "--set", "genre=Rock", "--yes"}},
		{args: []string{"edit", "--artist", "Nobody At All", "--set", "genre=Rock"}},
		{args: []string{"edit", "--batch", asset("edits.json"), "--dry-run"}},
		{args: []string{"edit", "--batch", asset("edits.json")}},
		{args: []string{"edit", "--batch", asset("edits.json"), "--yes"}},
		{args: []string{"credit", p(fx.one), "--role", "composer", "--name", "Comp", "--dry-run"}},
		{args: []string{"credit", p(fx.one), "--role", "composer", "--name", "Comp"}},
		{args: []string{"credit", "--batch", asset("credits.json"), "--dry-run"}},
		{args: []string{"credit", "--batch", asset("credits.json")}},
		{args: []string{"credit", "--batch", asset("credits.json"), "--yes"}},
		{args: []string{"tag", p(fx.one), "--key", "MOOD", "--value", "calm"}},
		{args: []string{"lock", p(fx.one), "title"}},
		{args: []string{"unlock", p(fx.one), "title"}},
		{args: []string{"acquisition", "set", p(fx.one), "--type", "manual"}},
		{args: []string{"acquisition", "clear", p(fx.one), "--force"}},
		{args: []string{"art", "set", p(fx.one), "--file", asset("cover.png")}},
		{args: []string{"art", p(fx.one)}},
		{args: []string{"art", "lock", p(fx.one), "--role", "back"}},
		{args: []string{"art", "unlock", p(fx.one), "--role", "back"}},
		{args: []string{"art", "set", p(fx.one), "--clear", "--role", "back"}},
		{args: []string{"lyrics", "set", p(fx.one), "--file", asset("one.lrc")}},
		{args: []string{"lyrics", "set", p(fx.one), "--clear", "--force"}},
		{args: []string{"chapters", "set", p(fx.book), "--file", asset("chapters.cue")}},
		{args: []string{"chapters", "set", p(fx.book), "--clear", "--force"}},
		{args: []string{"kind", p(fx.two), "--to", "book", "--dry-run"}},
		{args: []string{"kind", "--artist", "Artist", "--to", "book"}},
		{args: []string{"kind", "--artist", "Nobody At All", "--to", "book"}},
		{args: []string{"playlist", "create", "New"}},
		{args: []string{"playlist", "add", p(fx.mix), p(fx.three)}},
		{args: []string{"playlist", "remove", p(fx.mix), "--index", "0"}},
		{args: []string{"playlist", "rename", p(fx.mix), "Renamed"}},
		{args: []string{"playlist", "set-owner", p(fx.mix), p(fx.bob)}},
		{args: []string{"playlist", "import", "Imported", "--file", asset("mix.m3u8")}},
		{args: []string{"smartplaylist", "create", "Smart Two", "--rule", asset("rule.json")}},
		{args: []string{"smartplaylist", "set-rule", p(fx.smart), "--rule", asset("rule.json")}},
		{args: []string{"podcast", "transcript", p(fx.episode), "--file", asset("show.vtt"), "--format", "vtt"}},
		{args: []string{"podcast", "auth", p(fx.show), "listener", "--pass", "secret"}},
		{args: []string{"podcast", "retention", p(fx.show), "--keep", "5"}},
		{args: []string{"podcast", "retention", p(fx.show), "--apply"}},
		{args: []string{"podcast", "unfetch", p(fx.episode)}},
		{args: []string{"podcast", "add-episode", p(fx.show), "--title", "Ep Two", "--url", "https://example.invalid/two.mp3"}},
		{args: []string{"podcast", "add-manual", "Another Show"}},
		{args: []string{"podcast", "sync"}},
		{args: []string{"library", "set", p(fx.library), "--read-only"}},
		{args: []string{"library", "set", p(fx.library), "--writable"}},
		{args: []string{"library", "add", added + ":in-place"}},
		{args: []string{"library", "remove", "ADDED"}},
		{args: []string{"library", "remove", p(fx.library)}, wantErr: true},
		{args: []string{"merge", "artist", p(fx.artist), p(fx.other)}},
		{args: []string{"db", "vacuum"}},
		{args: []string{"db", "verify", "--fix"}},
		{args: []string{"db", "migrate"}},
		{args: []string{"analyze"}},
		{args: []string{"scan"}},
		{args: []string{"backup", asset("backup.db")}},
		{args: []string{"playlist", "delete", p(fx.mix)}},
		{args: []string{"podcast", "remove", p(fx.show)}},
		{args: []string{"detach", p(fx.one)}, wantErr: true},
		{args: []string{"mark-missing", p(fx.three)}},
		{args: []string{"kind", p(fx.three), "--to", "book"}},
		{args: []string{"podcast", "add", "https://example.invalid/feed.xml"}, wantErr: true},
		{args: []string{"podcast", "download", p(fx.episode)}, wantErr: true},
		{args: []string{"podcast", "transcript", p(fx.episode), "--fetch"}, wantErr: true},
		{args: []string{"opml", "import", asset("feeds.opml")}},
		{args: []string{"enrich", "--item", p(fx.one)}, wantErr: true},
		{args: []string{"db", "reseal-secrets"}, wantErr: true},
		{args: []string{"--db", filepath.Join(fx.assets, "fresh.db"), "init"}},
		{args: []string{"rm", p(fx.two), "--apply"}},
		{args: []string{"trash", "list"}},
		{args: []string{"trash", "restore", "TRASH"}},
		{args: []string{"rm", p(fx.two), "--apply"}},
		{args: []string{"trash", "purge", "TRASH"}},
		{args: []string{"organize", "--apply"}},
		{args: []string{"import", asset("acquired.mp3"), "--as", "track"}},
		{args: []string{"inbox", "import", asset("staging"), "--apply"}},
		{args: []string{"trash", "empty"}},
		{args: []string{"rebuild"}},
		{args: []string{"restore", asset("backup.db"), "--force"}, bare: true},
		{args: []string{"db", "reset", "--yes"}},
	}
	for _, c := range cases {
		// TRASH stands for the trash entry the last delete wrote, read off trash list, and
		// ADDED for the library `library add` registered.
		for i, a := range c.args {
			switch a {
			case "TRASH":
				c.args = append([]string(nil), c.args...)
				c.args[i] = lastTrashEntry(t, fx)
			case "ADDED":
				c.args = append([]string(nil), c.args...)
				c.args[i] = libraryAt(t, fx, added)
			}
		}
		stdout, stderr, err := runJSONCommand(fx, c.args, c.bare)
		name := strings.Join(c.args, " ")
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v (stderr %q), want error %v", name, err, stderr, c.wantErr)
			continue
		}
		if err != nil && strings.TrimSpace(stdout) == "" {
			continue
		}
		env, why := oneEnvelope(stdout)
		if why != "" {
			t.Errorf("%s: stdout is %s:\n%.300s", name, why, stdout)
			continue
		}
		if err != nil {
			var data map[string]json.RawMessage
			if json.Unmarshal(env["data"], &data) != nil || len(data["error"]) == 0 {
				t.Errorf("%s failed (%v) but printed a document with no error member:\n%.300s", name, err, stdout)
			}
		}
	}
}

// libraryAt reads the pid of the library at root off `library list --json`.
func libraryAt(t *testing.T, fx *jsonFixture, root string) string {
	t.Helper()
	stdout, _, err := runJSONCommand(fx, []string{"library", "list"}, false)
	if err != nil {
		t.Fatalf("library list: %v", err)
	}
	var env struct {
		Data []struct {
			PID  string `json:"pid"`
			Root string `json:"root"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("library list printed %q: %v", stdout, err)
	}
	for _, l := range env.Data {
		if l.Root == root {
			return l.PID
		}
	}
	t.Fatalf("no library at %s in %q", root, stdout)
	return ""
}

// lastTrashEntry reads the newest active trash entry's pid off `trash list --json`.
func lastTrashEntry(t *testing.T, fx *jsonFixture) string {
	t.Helper()
	stdout, _, err := runJSONCommand(fx, []string{"trash", "list"}, false)
	if err != nil {
		t.Fatalf("trash list: %v", err)
	}
	var env struct {
		Data []struct {
			PID string `json:"pid"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil || len(env.Data) == 0 {
		t.Fatalf("trash list printed %q (err %v), want an entry", stdout, err)
	}
	return env.Data[len(env.Data)-1].PID
}
