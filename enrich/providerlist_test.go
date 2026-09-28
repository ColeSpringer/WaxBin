package enrich_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/colespringer/waxbin/enrich"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// The per-pass provider list: Config.ProviderList hands the embedder the fixed list New
// assembled and uses whatever it returns, read once at the start of each pass.

// byNames picks providers out of a list by name, in the order the names are given,
// leaving out every provider not named. It is what a settings-driven hook does.
func byNames(list []enrich.Provider, names ...string) []enrich.Provider {
	var out []enrich.Provider
	for _, n := range names {
		for _, p := range list {
			if p != nil && p.Name() == n {
				out = append(out, p)
			}
		}
	}
	return out
}

// artistFrontProvider reads the provider stamped on one artist's front cover.
func artistFrontProvider(t *testing.T, dbPath, artist string) string {
	t.Helper()
	return scalarStr(t, roDB(t, dbPath), `SELECT COALESCE((SELECT am.provider FROM art_map am
		JOIN artist a ON a.id = am.entity_id
		WHERE am.entity_type = 'artist' AND am.role = 'front' AND a.name = ?), '')`, artist)
}

// frontOnlyArtistArt is an artist-art provider that answers every artist with a front
// and records who it was asked about.
func frontOnlyArtistArt(t *testing.T, name string, asked *[]string, onAsk func()) *enrich.Mock {
	return &enrich.Mock{ProviderName: name, Caps: enrich.CapArtistArt,
		EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
			*asked = append(*asked, req.Artist)
			if onAsk != nil {
				onAsk()
			}
			return &enrich.Candidate{Art: map[model.ArtRole]*model.ArtImage{
				model.ArtRoleFront: artImg(t, name+"-"+req.Artist),
			}}, nil
		}}
}

// TestProviderListIsReadOncePerPass: a list change made while a pass runs takes effect on
// the next pass, not halfway through this one, and the provenance names the provider
// that actually answered rather than a slot.
func TestProviderListIsReadOncePerPass(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "One", "Artist A", "Album A")
	seedTrack(t, st, lib.ID, "/lib/b.mp3", "ess-b", "Two", "Artist B", "Album B")

	current := "art-a"
	var askedA, askedB []string
	a := frontOnlyArtistArt(t, "art-a", &askedA, func() { current = "art-b" })
	b := frontOnlyArtistArt(t, "art-b", &askedB, nil)
	svc := enrich.New(st, enrich.Config{
		Providers: []enrich.Provider{a, b},
		ProviderList: func(fixed []enrich.Provider) []enrich.Provider {
			return byNames(fixed, current)
		},
	}, nil)

	first, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if first.ArtistArtEnriched != 2 {
		t.Fatalf("run 1 walked %d artists, want 2", first.ArtistArtEnriched)
	}
	if len(askedA) != 2 || len(askedB) != 0 {
		t.Fatalf("run 1 asked art-a %v and art-b %v, want art-a for both artists: the switch made on the first ask waits for the next pass", askedA, askedB)
	}

	seedTrack(t, st, lib.ID, "/lib/c.mp3", "ess-c", "Three", "Artist C", "Album C")
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if len(askedA) != 2 || len(askedB) != 1 || askedB[0] != "Artist C" {
		t.Fatalf("run 2 asked art-a %v and art-b %v, want art-b alone for Artist C", askedA, askedB)
	}
	for artist, want := range map[string]string{"Artist A": "art-a", "Artist B": "art-a", "Artist C": "art-b"} {
		if got := artistFrontProvider(t, dbPath, artist); got != want {
			t.Errorf("%s front provider = %q, want %q", artist, got, want)
		}
	}
}

// TestRunRefusesOnTheListItSnapshots: the Enabled read a caller makes before submitting a
// pass is advice. The pass decides on the list it snapshots, so a list emptied between
// the two refuses instead of walking nothing and reporting success, and a pass that saw
// a list walks it.
func TestRunRefusesOnTheListItSnapshots(t *testing.T) {
	ctx := context.Background()

	t.Run("a list emptied after the advice read refuses", func(t *testing.T) {
		st, dbPath, lib := openStore(t)
		seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "One", "Artist A", "Album A")
		var asked []string
		art := frontOnlyArtistArt(t, "art", &asked, nil)
		calls := 0
		svc := enrich.New(st, enrich.Config{
			Providers: []enrich.Provider{art},
			ProviderList: func(fixed []enrich.Provider) []enrich.Provider {
				calls++
				if calls == 1 {
					return fixed
				}
				return nil
			},
		}, nil)
		if !svc.Enabled() {
			t.Fatal("Enabled = false on the first read, want true: the list still holds the provider")
		}
		_, err := svc.Run(ctx, enrich.RunOptions{}, nil)
		if !waxerr.Is(err, waxerr.CodeUnsupported) {
			t.Fatalf("Run = %v, want CodeUnsupported: the pass's own list has nothing to run", err)
		}
		if len(asked) != 0 {
			t.Errorf("provider asked %v, want nothing", asked)
		}
		if n := scalarInt(t, roDB(t, dbPath), `SELECT COUNT(*) FROM entity_enrichment`); n != 0 {
			t.Errorf("markers written = %d, want none", n)
		}
	})

	t.Run("a pass that saw a list walks it", func(t *testing.T) {
		st, _, lib := openStore(t)
		seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "One", "Artist A", "Album A")
		var asked []string
		art := frontOnlyArtistArt(t, "art", &asked, nil)
		calls := 0
		svc := enrich.New(st, enrich.Config{
			Providers: []enrich.Provider{art},
			ProviderList: func(fixed []enrich.Provider) []enrich.Provider {
				calls++
				if calls == 1 {
					return fixed
				}
				return nil
			},
		}, nil)
		res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.ArtistArtEnriched != 1 || len(asked) != 1 {
			t.Fatalf("walked %d artists, asked %v; want the one artist, asked once: the pass acts on its one read", res.ArtistArtEnriched, asked)
		}
	})
}

// TestProviderListOrdersAndDropsTheBuiltIns: the built-ins are entries in the list like
// any other, so a hook moves them, drops them, and ranks the MusicBrainz genre baseline.
func TestProviderListOrdersAndDropsTheBuiltIns(t *testing.T) {
	ctx := context.Background()

	t.Run("an injected cover ranked after the archive loses the front to it", func(t *testing.T) {
		rgFrontProvider := func(t *testing.T, list func([]enrich.Provider) []enrich.Provider) string {
			t.Helper()
			st, dbPath, lib := openStore(t)
			seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
			caa, _ := newCAAMock(t, pngBytes(t))
			fanart := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapCover,
				EnrichFunc: func(_ context.Context, req enrich.Request) (*enrich.Candidate, error) {
					if req.Type != enrich.TargetReleaseGroup {
						return nil, nil
					}
					return &enrich.Candidate{Cover: artImg(t, "fanart-front")}, nil
				}}
			svc := enrich.New(st, enrich.Config{
				Contact: "t@e.com", MinRequestInterval: time.Millisecond, FetchCoverArt: true,
				MusicBrainzBaseURL: mbMockGenres(t, `[]`).URL, CoverArtBaseURL: caa.URL,
				Providers: []enrich.Provider{fanart}, ProviderList: list,
			}, nil)
			if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
				t.Fatalf("Run: %v", err)
			}
			return scalarStr(t, roDB(t, dbPath), `SELECT COALESCE((SELECT provider FROM art_map
				WHERE entity_type = 'release_group' AND role = 'front'), '')`)
		}
		if got := rgFrontProvider(t, nil); got != "fanart" {
			t.Fatalf("fixed order front provider = %q, want fanart (injected ahead of the built-ins)", got)
		}
		archiveFirst := func(fixed []enrich.Provider) []enrich.Provider {
			return byNames(fixed, "coverartarchive", "fanart", "musicbrainz")
		}
		if got := rgFrontProvider(t, archiveFirst); got != "coverartarchive" {
			t.Errorf("reordered front provider = %q, want coverartarchive", got)
		}
	})

	t.Run("a list without lrclib walks no lyrics", func(t *testing.T) {
		st, dbPath, lib := openStore(t)
		seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
		lrc := lrclibMock(t, "", "shine on")
		svc := enrich.New(st, enrich.Config{
			Contact: "t@e.com", MinRequestInterval: time.Millisecond, FetchLyrics: true,
			MusicBrainzBaseURL: mbMockGenres(t, `[]`).URL, LRCLibBaseURL: lrc.URL,
			ProviderList: func(fixed []enrich.Provider) []enrich.Provider {
				return byNames(fixed, "musicbrainz")
			},
		}, nil)
		res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.LyricsEnriched != 0 {
			t.Errorf("lyrics walked %d tracks, want 0 with lrclib out of the list", res.LyricsEnriched)
		}
		if n := scalarInt(t, roDB(t, dbPath), `SELECT COUNT(*) FROM entity_enrichment WHERE entity_type = 'lyrics'`); n != 0 {
			t.Errorf("lyrics markers = %d, want none", n)
		}
	})

	genreRun := func(t *testing.T, names ...string) (string, string, []string) {
		t.Helper()
		st, dbPath, lib := openStore(t)
		item := seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
		mb := mbMockGenres(t, `[{"name":"Progressive Rock","count":5}]`)
		lb := lbMock(t, "wywh-mbid", `[{"tag":"art rock","count":9,"genre_mbid":"g1"}]`)
		svc := enrich.New(st, enrich.Config{
			Contact: "t@e.com", MinRequestInterval: time.Millisecond, FetchCommunityGenres: true,
			MusicBrainzBaseURL: mb.URL, ListenBrainzBaseURL: lb.URL,
			ProviderList: func(fixed []enrich.Provider) []enrich.Provider {
				return byNames(fixed, names...)
			},
		}, nil)
		if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
			t.Fatalf("Run: %v", err)
		}
		db := roDB(t, dbPath)
		display := scalarStr(t, db, `SELECT COALESCE(t.genre,'') FROM track t JOIN playable_item pi ON pi.id = t.item_id WHERE pi.pid = ?`, string(item))
		rows, err := db.Query(`SELECT gn.name FROM item_genre ig JOIN genre gn ON gn.id = ig.genre_id
			JOIN playable_item pi ON pi.id = ig.item_id WHERE pi.pid = ? ORDER BY gn.name`, string(item))
		if err != nil {
			t.Fatalf("item genres: %v", err)
		}
		defer rows.Close()
		var attached []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatalf("scan genre: %v", err)
			}
			attached = append(attached, n)
		}
		return display, genreProvenanceProvider(t, dbPath, item), attached
	}

	t.Run("listenbrainz ranked ahead of musicbrainz takes the primary genre", func(t *testing.T) {
		display, provider, _ := genreRun(t, "listenbrainz", "musicbrainz")
		if display != "art rock; Progressive Rock" {
			t.Errorf("track.genre = %q, want the community tag first and the spine's genre second", display)
		}
		if provider != "listenbrainz" {
			t.Errorf("genre provenance = %q, want listenbrainz", provider)
		}
	})

	t.Run("a list without musicbrainz leaves the spine's genres out", func(t *testing.T) {
		display, provider, attached := genreRun(t, "listenbrainz")
		if display != "art rock" || provider != "listenbrainz" {
			t.Errorf("track.genre = %q from %q, want art rock from listenbrainz alone", display, provider)
		}
		if len(attached) != 1 || attached[0] != "art rock" {
			t.Errorf("attached genres = %v, want [art rock]", attached)
		}
	})
}

// TestProviderListDropsNilNamelessAndDuplicateEntries: a hook's answer is filtered the
// way Providers is, so a nil entry cannot panic the pass, a nameless provider cannot
// write values with no provenance, and a provider listed twice is asked once.
func TestProviderListDropsNilNamelessAndDuplicateEntries(t *testing.T) {
	ctx := context.Background()
	st, _, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "One", "Artist A", "Album A")
	seedTrack(t, st, lib.ID, "/lib/b.mp3", "ess-b", "Two", "Artist B", "Album B")

	var namelessAsked, lyricsAsked int
	nameless := &enrich.Mock{Caps: enrich.CapLyrics,
		EnrichFunc: func(context.Context, enrich.Request) (*enrich.Candidate, error) {
			namelessAsked++
			return nil, nil
		}}
	lyrics := &enrich.Mock{ProviderName: "lyrics", Caps: enrich.CapLyrics,
		EnrichFunc: func(context.Context, enrich.Request) (*enrich.Candidate, error) {
			lyricsAsked++
			return nil, nil // a miss, so a second listing would be asked too
		}}
	svc := enrich.New(st, enrich.Config{
		ProviderList: func([]enrich.Provider) []enrich.Provider {
			return []enrich.Provider{nil, nameless, lyrics, lyrics}
		},
	}, nil)
	res, err := svc.Run(ctx, enrich.RunOptions{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.LyricsEnriched != 2 {
		t.Fatalf("lyrics walked %d tracks, want 2", res.LyricsEnriched)
	}
	if namelessAsked != 0 {
		t.Errorf("nameless provider asked %d times, want 0", namelessAsked)
	}
	if lyricsAsked != 2 {
		t.Errorf("lyrics provider asked %d times, want 2 (once per track)", lyricsAsked)
	}
}

// TestBuiltinsListsWhatTheInstallRegistered: a settings surface can list what the
// install has on offer before any pass runs, the MusicBrainz genre entry included.
func TestBuiltinsListsWhatTheInstallRegistered(t *testing.T) {
	injected := &enrich.Mock{ProviderName: "fanart", Caps: enrich.CapCover}
	names := func(ps []enrich.Provider) []string {
		out := make([]string, len(ps))
		for i, p := range ps {
			out[i] = p.Name()
		}
		return out
	}
	full := enrich.New(nil, enrich.Config{
		Contact: "t@e.com", FetchCoverArt: true, FetchCommunityGenres: true, FetchLyrics: true,
		Providers: []enrich.Provider{injected},
	}, nil)
	got := names(full.Builtins())
	want := []string{"coverartarchive", "musicbrainz", "listenbrainz", "lrclib"}
	if len(got) != len(want) {
		t.Fatalf("builtins = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("builtins = %v, want %v", got, want)
		}
	}

	bare := enrich.New(nil, enrich.Config{Contact: "t@e.com"}, nil)
	if got := names(bare.Builtins()); len(got) != 1 || got[0] != "musicbrainz" {
		t.Errorf("builtins with every toggle off = %v, want [musicbrainz]", got)
	}

	contactless := enrich.New(nil, enrich.Config{
		FetchCoverArt: true, FetchCommunityGenres: true, FetchLyrics: true,
		Providers: []enrich.Provider{injected},
	}, nil)
	if got := contactless.Builtins(); len(got) != 0 {
		t.Errorf("builtins without a contact = %v, want none", names(got))
	}
}

// TestInjectedProvidersCannotTakeABuiltinsName: a built-in's name is its provenance mark,
// and musicbrainz and none also label markers, so an injected provider claiming one would
// write values nobody could tell from the built-in's. It is dropped at New, loudly, and
// the built-in answers as usual.
func TestInjectedProvidersCannotTakeABuiltinsName(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"lrclib", "musicbrainz", "none"} {
		t.Run(name, func(t *testing.T) {
			st, _, lib := openStore(t)
			item := seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
			asked := 0
			impostor := &enrich.Mock{ProviderName: name, Caps: enrich.CapLyrics | enrich.CapGenres,
				EnrichFunc: func(context.Context, enrich.Request) (*enrich.Candidate, error) {
					asked++
					return &enrich.Candidate{Lyrics: &model.Lyrics{Unsynced: "impostor"}, Genres: []string{"Impostor"}}, nil
				}}
			var logs warnings
			svc := enrich.New(st, enrich.Config{
				Contact: "t@e.com", MinRequestInterval: time.Millisecond, FetchLyrics: true,
				MusicBrainzBaseURL: mbMockGenres(t, `[{"name":"Progressive Rock","count":5}]`).URL,
				LRCLibBaseURL:      lrclibMock(t, "", "shine on").URL,
				Providers:          []enrich.Provider{impostor},
			}, slog.New(&logs))
			if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if asked != 0 {
				t.Errorf("the provider named %q was asked %d times, want never", name, asked)
			}
			if ly, err := st.LyricsByItem(ctx, item); err != nil || ly.Provider != "lrclib" || ly.Unsynced != "shine on" {
				t.Errorf("lyrics = %+v (err %v), want the built-in LRCLIB's", ly, err)
			}
			if len(logs.msgs) == 0 {
				t.Error("dropping the provider logged no warning")
			}
		})
	}
}

// TestSameNamedInjectedProvidersKeepTheFirst: two injected providers under one name would
// share its provenance and its failure count, so the second is dropped at New.
func TestSameNamedInjectedProvidersKeepTheFirst(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "One", "Artist A", "Album A")
	var firstAsked, secondAsked []string
	first := frontOnlyArtistArt(t, "fanart", &firstAsked, nil)
	second := frontOnlyArtistArt(t, "fanart", &secondAsked, nil)
	var logs warnings
	svc := enrich.New(st, enrich.Config{MinRequestInterval: time.Millisecond, Providers: []enrich.Provider{first, second}}, slog.New(&logs))
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(firstAsked) != 1 || len(secondAsked) != 0 {
		t.Errorf("asked the first %d and the second %d times, want the first once and the second never", len(firstAsked), len(secondAsked))
	}
	if got := artistFrontProvider(t, dbPath, "Artist A"); got != "fanart" {
		t.Errorf("front provider = %q, want fanart", got)
	}
	if len(logs.msgs) == 0 {
		t.Error("dropping the second provider logged no warning")
	}
}

// TestNewDropsANilInjectedProvider: a nil entry in the injected list is dropped the way
// the per-pass list drops one, rather than panicking the service's construction (and with
// it the library's Open).
func TestNewDropsANilInjectedProvider(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStore(t)
	seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "One", "Artist A", "Album A")
	var asked []string
	fanart := frontOnlyArtistArt(t, "fanart", &asked, nil)
	svc := enrich.New(st, enrich.Config{MinRequestInterval: time.Millisecond, Providers: []enrich.Provider{nil, fanart}}, nil)
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(asked) != 1 {
		t.Errorf("asked the provider after the nil entry %d times, want once", len(asked))
	}
	if got := artistFrontProvider(t, dbPath, "Artist A"); got != "fanart" {
		t.Errorf("front provider = %q, want fanart", got)
	}
}

// TestProviderListDropsAnImpostorOfABuiltin: a hook may move or drop a built-in, but an
// entry it adds under a built-in's name is not that built-in, so it is dropped and the
// registered one keeps its place.
func TestProviderListDropsAnImpostorOfABuiltin(t *testing.T) {
	ctx := context.Background()
	st, _, lib := openStore(t)
	item := seedTrack(t, st, lib.ID, "/lib/a.mp3", "ess-a", "Shine On", "Pink Floyd", "Wish You Were Here")
	asked := 0
	impostor := &enrich.Mock{ProviderName: "lrclib", Caps: enrich.CapLyrics,
		EnrichFunc: func(context.Context, enrich.Request) (*enrich.Candidate, error) {
			asked++
			return &enrich.Candidate{Lyrics: &model.Lyrics{Unsynced: "impostor"}}, nil
		}}
	svc := enrich.New(st, enrich.Config{
		Contact: "t@e.com", MinRequestInterval: time.Millisecond, FetchLyrics: true,
		MusicBrainzBaseURL: mbMockGenres(t, `[]`).URL, LRCLibBaseURL: lrclibMock(t, "", "shine on").URL,
		ProviderList: func(fixed []enrich.Provider) []enrich.Provider {
			return append([]enrich.Provider{impostor}, fixed...)
		},
	}, nil)
	if _, err := svc.Run(ctx, enrich.RunOptions{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if asked != 0 {
		t.Errorf("the added lrclib was asked %d times, want never", asked)
	}
	if ly, err := st.LyricsByItem(ctx, item); err != nil || ly.Unsynced != "shine on" {
		t.Errorf("lyrics = %+v (err %v), want the built-in LRCLIB's", ly, err)
	}
}
