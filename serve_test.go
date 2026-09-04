package waxbin_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/internal/testsock"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/podcast"
	"github.com/colespringer/waxbin/proxy"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/scan"
	"github.com/colespringer/waxbin/waxerr"
)

// serveLib starts Serve on an already-open read-write library in the background and
// tears it down at cleanup.
func serveLib(t *testing.T, ctx context.Context, lib *waxbin.Library, sock string) {
	t.Helper()
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- lib.Serve(sctx, sock) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve returned: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("serve did not stop")
		}
		_ = lib.Close()
	})
}

// openServedRW opens a read-write managed library advertising a control socket
// (without scanning) and starts Serve. The caller drives the catalog.
func openServedRW(t *testing.T, ctx context.Context, db, root, sock string) *waxbin.Library {
	t.Helper()
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath:    db,
		Roots:     []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
		IPCSocket: sock,
	})
	if err != nil {
		t.Fatalf("open served library: %v", err)
	}
	serveLib(t, ctx, lib, sock)
	return lib
}

// openServed opens a read-write managed library advertising a control socket,
// scans root, and starts Serve in the background. It returns the library and the
// socket path; the server is stopped and the library closed at cleanup.
func openServed(t *testing.T, ctx context.Context, db, root, sock string) *waxbin.Library {
	t.Helper()
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath:    db,
		Roots:     []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
		IPCSocket: sock,
	})
	if err != nil {
		t.Fatalf("open served library: %v", err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		_ = lib.Close()
		t.Fatalf("scan: %v", err)
	}
	serveLib(t, ctx, lib, sock)
	return lib
}

// dialWhenReady dials the server socket, retrying until it answers a ping.
func dialWhenReady(t *testing.T, sock string) *proxy.Client {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		c, err := proxy.Dial(sock)
		if err == nil {
			if perr := c.Ping(context.Background()); perr == nil {
				t.Cleanup(func() { _ = c.Close() })
				return c
			} else {
				lastErr = perr
			}
			_ = c.Close()
		} else {
			lastErr = err
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server not ready on %s: %v", sock, lastErr)
	return nil
}

// TestServeProxiedMutations drives the fast-mutation proxy end to end: with a
// server holding the write lock, a client's edits, ratings, stars, and user
// creation all succeed through the socket instead of failing with CodeConflict.
func TestServeProxiedMutations(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	sock := testsock.Path(t)
	writeFile(t, filepath.Join(root, "song.mp3"), testaudio.BuildMP3("Original", "Old Artist", "Album", 1))

	lib := openServed(t, ctx, db, root, sock)
	pid := itemPIDByTitle(t, ctx, lib, "Original")

	// The lockfile advertises the socket, so a CLI can discover the server.
	if info, err := waxbin.ReadLockOwner(db); err != nil || info.IPCSocket != sock {
		t.Fatalf("lock owner = %+v (err %v), want IPCSocket %s", info, err, sock)
	}

	c := dialWhenReady(t, sock)

	// A field edit succeeds through the proxy while the server holds the lock.
	res, err := c.EditFields(ctx, pid, map[string]string{"artist": "New Artist"}, false,
		model.Attribution{}, model.LockOn, false)
	if err != nil {
		t.Fatalf("proxied edit: %v", err)
	}
	if len(res.WriteBackFailures) != 0 {
		t.Fatalf("unexpected write-back failures: %+v", res.WriteBackFailures)
	}
	// The catalog reflects it (read through the server's own library).
	if v, err := lib.Get(ctx, pid); err != nil || v.Artist != "New Artist" {
		t.Fatalf("catalog artist = %q (err %v), want New Artist", v.Artist, err)
	}
	// Provenance (read through the proxy) records a locked user edit.
	prov, err := c.Provenance(ctx, pid)
	if err != nil {
		t.Fatalf("proxied provenance: %v", err)
	}
	if len(prov) != 1 || prov[0].Field != "artist" || prov[0].Source != model.SourceUser || !prov[0].Locked {
		t.Fatalf("provenance = %+v, want one locked user artist row", prov)
	}

	// A scalar edit carries its attribution across the socket too: an embedder that
	// fetched the value is the other half of the bug the art fields close, and a
	// LockUnchanged write leaves the lock the first edit set.
	if _, err := c.EditFields(ctx, pid, map[string]string{"comment": "fetched"}, false,
		model.Attribution{Source: model.SourceEnrichment, Provider: "itunes"}, model.LockUnchanged, false); err != nil {
		t.Fatalf("proxied stamped edit: %v", err)
	}
	prov, err = c.Provenance(ctx, pid)
	if err != nil {
		t.Fatalf("proxied provenance: %v", err)
	}
	var comment *model.FieldProvenance
	for i := range prov {
		if prov[i].Field == "comment" {
			comment = &prov[i]
		}
	}
	if comment == nil || comment.Source != model.SourceEnrichment || comment.Provider != "itunes" || comment.Locked {
		t.Fatalf("comment provenance = %+v, want an unlocked itunes row", comment)
	}
	if prov[0].Field != "artist" || !prov[0].Locked {
		t.Errorf("artist row = %+v, want the earlier lock still standing", prov[0])
	}

	// Play-state mutations round-trip.
	rating := 80
	if _, err := c.SetRating(ctx, "", pid, &rating, nil); err != nil {
		t.Fatalf("proxied rating: %v", err)
	}
	if _, err := c.SetStar(ctx, "", pid, true, nil); err != nil {
		t.Fatalf("proxied star: %v", err)
	}
	st, err := c.PlayState(ctx, "", pid)
	if err != nil {
		t.Fatalf("proxied play state: %v", err)
	}
	if !st.HasRating || st.Rating != 80 || !st.Starred {
		t.Fatalf("play state = %+v, want rating 80 + starred", st)
	}

	// The as-of stamp rides the wire (asOfNs): an unstar recorded far in the past,
	// older than the server-now star just applied, is skipped as a stale replay, so
	// the item stays starred. This exercises the client encode, server decode, and
	// store recorded-time guard end to end.
	oldNS := int64(1_000_000_000) // 1970, older than the server-now star above
	if _, err := c.SetStar(ctx, "", pid, false, &oldNS); err != nil {
		t.Fatalf("proxied stale unstar: %v", err)
	}
	if st, err = c.PlayState(ctx, "", pid); err != nil {
		t.Fatalf("proxied play state after stale unstar: %v", err)
	}
	if !st.Starred {
		t.Fatalf("stale as-of unstar was applied over the wire, want skipped (still starred)")
	}

	// User creation round-trips and is visible in the listing.
	u, err := c.CreateUser(ctx, "Bob")
	if err != nil {
		t.Fatalf("proxied create user: %v", err)
	}
	users, err := c.Users(ctx)
	if err != nil {
		t.Fatalf("proxied users: %v", err)
	}
	if !hasUser(users, u.PID, "Bob") {
		t.Fatalf("users = %+v, want one named Bob", users)
	}
}

// TestServeProxiedCreditsBatch drives set_credits_batch end to end: several credits
// applied as one atomic batch over the wire, one item's two roles arriving as distinct
// result entries, a repeated (item, role) pair refused, and a locked entry either
// failing the batch or being reported as skipped with its role.
func TestServeProxiedCreditsBatch(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	sock := testsock.Path(t)
	writeFile(t, filepath.Join(root, "one.mp3"),
		testaudio.BuildMP3WithAudio("One", "Old Artist", "Album", 1, testaudio.AudioWithSeed(1)))
	writeFile(t, filepath.Join(root, "two.mp3"),
		testaudio.BuildMP3WithAudio("Two", "Old Artist", "Album", 2, testaudio.AudioWithSeed(2)))

	lib := openServed(t, ctx, db, root, sock)
	p1 := itemPIDByTitle(t, ctx, lib, "One")
	p2 := itemPIDByTitle(t, ctx, lib, "Two")
	c := dialWhenReady(t, sock)

	items := []proxy.ItemCreditsEdit{
		{ItemPID: string(p1), Role: string(model.RoleArtist), Names: []string{"New Artist"}},
		{ItemPID: string(p1), Role: string(model.RoleComposer), Names: []string{"New Composer"}},
		{ItemPID: string(p2), Role: string(model.RoleArtist), Names: []string{"New Artist"}},
	}
	res, err := c.SetCreditsBatch(ctx, items, false, model.Attribution{}, model.LockOn, false, false)
	if err != nil {
		t.Fatalf("proxied credit batch: %v", err)
	}
	if len(res.Edited) != 3 || len(res.Skipped) != 0 {
		t.Fatalf("result = %+v, want all three entries edited", res)
	}
	// The first item was edited under two roles: the wire result names each entry's
	// role and stored names, which a pid list could not tell apart.
	p1Names := map[string][]string{}
	for _, e := range res.Edited {
		if e.ItemPID == string(p1) {
			p1Names[e.Role] = e.Names
		}
	}
	if len(p1Names) != 2 || len(p1Names[string(model.RoleArtist)]) != 1 ||
		len(p1Names[string(model.RoleComposer)]) != 1 || p1Names[string(model.RoleComposer)][0] != "New Composer" {
		t.Fatalf("edited = %+v, want the first item's artist and composer entries distinct with their stored names", res.Edited)
	}
	for _, pid := range []model.PID{p1, p2} {
		if v, err := lib.Get(ctx, pid); err != nil || v.Artist != "New Artist" {
			t.Fatalf("%s artist = %q (err %v), want New Artist", pid, v.Artist, err)
		}
	}

	// A repeated pair is a caller bug, refused before any write.
	if _, err := c.SetCreditsBatch(ctx, []proxy.ItemCreditsEdit{items[0], items[0]},
		false, model.Attribution{}, model.LockOn, true, false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("repeated pair = %v, want CodeInvalid", err)
	}

	// The first batch locked credit.artist, so a second run is refused, and skipLocked
	// reports it instead of failing.
	relock := []proxy.ItemCreditsEdit{
		{ItemPID: string(p1), Role: string(model.RoleArtist), Names: []string{"Another"}},
	}
	if _, err := c.SetCreditsBatch(ctx, relock, false, model.Attribution{}, model.LockOn, false, false); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("locked entry = %v, want CodeLocked", err)
	}
	res, err = c.SetCreditsBatch(ctx, relock, false, model.Attribution{}, model.LockOn, false, true)
	if err != nil {
		t.Fatalf("proxied skip-locked batch: %v", err)
	}
	if len(res.Edited) != 0 || len(res.Skipped) != 1 ||
		res.Skipped[0].ItemPID != string(p1) || res.Skipped[0].Role != string(model.RoleArtist) {
		t.Fatalf("result = %+v, want the locked entry skipped with its role", res)
	}
}

// albumPIDFromFacet returns the pid of the first album entity in the catalog, read
// through the album facet, the same enumeration WaxDeck uses to find one.
func albumPIDFromFacet(t *testing.T, ctx context.Context, lib *waxbin.Library) model.PID {
	t.Helper()
	fr, err := lib.Facet(ctx, query.New(query.EntityItems).Build(), read.GroupAlbum, "", 0, "")
	if err != nil {
		t.Fatalf("album facet: %v", err)
	}
	for _, b := range fr.Buckets {
		if b.EntityPID != "" {
			return b.EntityPID
		}
	}
	t.Fatal("no album entity pid from the album facet")
	return ""
}

// TestServeProxiedEntityStar drives set_entity_star / set_entity_rating end to end: an
// album entity is starred and rated over the wire and read back through the served
// library, and the as-of stamp rides asOfNs so a stale replay is skipped by the recorded
// time guard, the same wire fields and new methods WaxDeck's getStarred2 import uses.
func TestServeProxiedEntityStar(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	sock := testsock.Path(t)
	writeFile(t, filepath.Join(root, "song.mp3"), testaudio.BuildMP3("Original", "Old Artist", "Album", 1))

	lib := openServed(t, ctx, db, root, sock)
	albumPID := albumPIDFromFacet(t, ctx, lib)
	c := dialWhenReady(t, sock)

	// Star and rate the album entity over the wire; read back through the served library
	// (entity state has no proxy read method by design, so the read is in-process).
	if _, err := c.SetEntityStar(ctx, "", model.MergeAlbum, albumPID, true, nil); err != nil {
		t.Fatalf("proxied entity star: %v", err)
	}
	rating := 90
	if _, err := c.SetEntityRating(ctx, "", model.MergeAlbum, albumPID, &rating, nil); err != nil {
		t.Fatalf("proxied entity rating: %v", err)
	}
	st, err := lib.EntityPlayState(ctx, "", model.MergeAlbum, albumPID)
	if err != nil {
		t.Fatalf("read entity state: %v", err)
	}
	if !st.Starred || !st.HasRating || st.Rating != 90 {
		t.Fatalf("entity state = %+v, want starred + rating 90", st)
	}

	// The as-of stamp rides asOfNs: an unstar recorded far in the past (older than the
	// server-now star above) is skipped as a stale replay, so the album stays starred.
	// This exercises the client encode, server decode, and store guard end to end.
	oldNS := int64(1_000_000_000) // 1970
	if _, err := c.SetEntityStar(ctx, "", model.MergeAlbum, albumPID, false, &oldNS); err != nil {
		t.Fatalf("proxied stale entity unstar: %v", err)
	}
	if st, err = lib.EntityPlayState(ctx, "", model.MergeAlbum, albumPID); err != nil {
		t.Fatalf("read entity state after stale unstar: %v", err)
	}
	if !st.Starred {
		t.Fatalf("stale as-of entity unstar was applied over the wire (state %+v), want skipped", st)
	}
}

// TestServeProxiedChangedBool drives the changed bool of the star/rating methods and
// set_played over the wire: a real flip reports true, an immediate value-identical
// repeat reports false, for both the item and the entity twins. It also pins why the
// protocol version
// had to move: a version-4 frame carries no result payload, which would decode as
// changed=false and make every proxied write look like a no-op, so the server must refuse
// it outright rather than answer it.
func TestServeProxiedChangedBool(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	sock := testsock.Path(t)
	writeFile(t, filepath.Join(root, "song.mp3"), testaudio.BuildMP3("Original", "Old Artist", "Album", 1))

	lib := openServed(t, ctx, db, root, sock)
	pid := itemPIDByTitle(t, ctx, lib, "Original")
	c := dialWhenReady(t, sock)

	rating := 80
	if changed, err := c.SetRating(ctx, "", pid, &rating, nil); err != nil || !changed {
		t.Fatalf("first proxied rating: changed=%v err=%v, want true", changed, err)
	}
	if changed, err := c.SetRating(ctx, "", pid, &rating, nil); err != nil || changed {
		t.Fatalf("identical proxied re-rate: changed=%v err=%v, want false", changed, err)
	}
	if changed, err := c.SetStar(ctx, "", pid, true, nil); err != nil || !changed {
		t.Fatalf("first proxied star: changed=%v err=%v, want true", changed, err)
	}
	if changed, err := c.SetStar(ctx, "", pid, true, nil); err != nil || changed {
		t.Fatalf("proxied re-star: changed=%v err=%v, want false", changed, err)
	}

	// set_played returns the same payload, so it rides the same assertion.
	if changed, err := c.SetPlayed(ctx, "", pid, true, true, nil, nil); err != nil || !changed {
		t.Fatalf("first proxied set_played: changed=%v err=%v, want true", changed, err)
	}
	if changed, err := c.SetPlayed(ctx, "", pid, true, true, nil, nil); err != nil || changed {
		t.Fatalf("identical proxied set_played: changed=%v err=%v, want false", changed, err)
	}

	albumPID := albumPIDFromFacet(t, ctx, lib)
	if changed, err := c.SetEntityStar(ctx, "", model.MergeAlbum, albumPID, true, nil); err != nil || !changed {
		t.Fatalf("first proxied entity star: changed=%v err=%v, want true", changed, err)
	}
	if changed, err := c.SetEntityStar(ctx, "", model.MergeAlbum, albumPID, true, nil); err != nil || changed {
		t.Fatalf("proxied entity re-star: changed=%v err=%v, want false", changed, err)
	}
	if changed, err := c.SetEntityRating(ctx, "", model.MergeAlbum, albumPID, &rating, nil); err != nil || !changed {
		t.Fatalf("first proxied entity rating: changed=%v err=%v, want true", changed, err)
	}
	if changed, err := c.SetEntityRating(ctx, "", model.MergeAlbum, albumPID, &rating, nil); err != nil || changed {
		t.Fatalf("identical proxied entity re-rate: changed=%v err=%v, want false", changed, err)
	}

	// A hand-built version-4 set_star frame: refused with a typed error, so a client
	// speaking the older protocol gets a distinct failure instead of a silent false.
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("raw dial: %v", err)
	}
	defer conn.Close()
	frame := fmt.Sprintf(`{"v":4,"method":%q,"params":{"userPid":"","itemPid":%q,"starred":false}}`+"\n",
		proxy.MethodSetStar, pid)
	if _, err := conn.Write([]byte(frame)); err != nil {
		t.Fatalf("raw write: %v", err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("raw read: %v", err)
	}
	var resp struct {
		OK    bool `json:"ok"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("unmarshal %q: %v", line, err)
	}
	if resp.OK || resp.Error.Code != string(waxerr.CodeInvalid) {
		t.Fatalf("v4 set_star response = %+v, want a CodeInvalid rejection", resp)
	}
	// The refused frame must not have unstarred the item.
	if st, err := c.PlayState(ctx, "", pid); err != nil || !st.Starred {
		t.Fatalf("play state after the refused v4 unstar = %+v (err %v), want still starred", st, err)
	}
}

// TestServeProxiedMarkMissing drives mark_missing over the wire, which is the point
// of proxying it at all: the server holds the write lock, so a client that finds a
// vanished file has no other way to tell the catalog while `waxbin serve` is up. The
// verification runs on the server's filesystem, so the refusal travels too.
func TestServeProxiedMarkMissing(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	sock := testsock.Path(t)
	path := filepath.Join(root, "song.mp3")
	writeFile(t, path, testaudio.BuildMP3("Original", "Old Artist", "Album", 1))

	lib := openServed(t, ctx, db, root, sock)
	pid := itemPIDByTitle(t, ctx, lib, "Original")
	c := dialWhenReady(t, sock)

	outcome, err := c.MarkMissing(ctx, pid, false)
	if err != nil {
		t.Fatalf("proxied mark with the file present: %v", err)
	}
	if outcome != model.OutcomeFilesPresent {
		t.Fatalf("outcome = %q, want files-present", outcome)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if outcome, err = c.MarkMissing(ctx, pid, false); err != nil || outcome != model.OutcomeMarked {
		t.Fatalf("proxied mark after deletion = %q (err %v), want marked", outcome, err)
	}
	if st := stateOf(t, ctx, lib, pid); st != model.StateMissing {
		t.Fatalf("state = %q, want missing", st)
	}
	if outcome, err = c.MarkMissing(ctx, pid, true); err != nil || outcome != model.OutcomeAlreadyMissing {
		t.Fatalf("proxied forced re-mark = %q (err %v), want already-missing", outcome, err)
	}
}

// TestServeProxiedSmartPlaylistSetRule drives playlist_set_rule end to end: the
// rule is replaced under the same pid, membership follows on the next read, and a
// bad rule keeps its CodeInvalid class across the wire.
func TestServeProxiedSmartPlaylistSetRule(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	sock := testsock.Path(t)
	writeFile(t, filepath.Join(root, "song.mp3"), testaudio.BuildMP3("Original", "Old Artist", "Album", 1))

	lib := openServed(t, ctx, db, root, sock)
	c := dialWhenReady(t, sock)

	// A rule matching nothing, replaced over the socket by one matching the
	// scanned track.
	empty := query.New(query.EntityItems).Where("title", query.OpContains, "Nothing").Build()
	pid, err := lib.Playlists().CreateSmart(ctx, "Mix", "", "", empty)
	if err != nil {
		t.Fatalf("create smart: %v", err)
	}
	if items, err := lib.Playlists().Items(ctx, pid, ""); err != nil || len(items) != 0 {
		t.Fatalf("initial membership = %d items (err %v), want 0", len(items), err)
	}

	match := query.New(query.EntityItems).Where("title", query.OpContains, "Origin").Build()
	doc, err := query.MarshalRule(match)
	if err != nil {
		t.Fatalf("marshal rule: %v", err)
	}
	if err := c.PlaylistSetRule(ctx, pid, doc); err != nil {
		t.Fatalf("proxied set-rule: %v", err)
	}
	items, err := lib.Playlists().Items(ctx, pid, "")
	if err != nil || len(items) != 1 || items[0].Title != "Original" {
		t.Fatalf("membership after proxied set-rule = %v (err %v), want [Original] under the same pid", items, err)
	}

	// A rule the store rejects keeps its class across the wire.
	bad, err := query.MarshalRule(query.New(query.EntityItems).Where("bogus", query.OpIs, "x").Build())
	if err != nil {
		t.Fatalf("marshal bad rule: %v", err)
	}
	if err := c.PlaylistSetRule(ctx, pid, bad); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("proxied bad rule err = %v, want CodeInvalid", err)
	}
}

// TestServeProxiedPlaylistArt drives set_entity_art for a playlist over the socket:
// the entity type rides the existing wire field (no new method, no version bump), the
// cover resolves at the playlist's own level through the served library, and the
// HasArt projection follows. This is the surface WaxDeck serves a synced playlist's
// cover from.
func TestServeProxiedPlaylistArt(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "catalog.db")
	sock := testsock.Path(t)

	// A playlist cover needs no catalog behind it, so this one skips the scan.
	lib := openServedRW(t, ctx, db, t.TempDir(), sock)
	pid, err := lib.Playlists().CreateStatic(ctx, "Mix", "", "")
	if err != nil {
		t.Fatalf("create playlist: %v", err)
	}
	c := dialWhenReady(t, sock)

	cover := coverPNG(t)
	// Write-back is on to prove a playlist has no on-disk fan-out to fail at: there is
	// no file behind a playlist, so the flag is a clean no-op rather than an error.
	res, err := c.SetEntityArt(ctx, model.ArtPlaylist, pid, model.ArtRoleFront, cover, "",
		model.Attribution{}, model.LockOff, false, true)
	if err != nil {
		t.Fatalf("proxied playlist art: %v", err)
	}
	if len(res.WriteBackFailures) != 0 {
		t.Fatalf("write-back failures for a playlist cover: %+v, want none", res.WriteBackFailures)
	}

	blob, err := lib.ResolveArt(ctx, model.EntityRef{Type: model.ArtPlaylist, PID: pid}, model.ArtRoleFront, 0)
	if err != nil {
		t.Fatalf("resolve playlist art: %v", err)
	}
	if blob.Level != model.ArtPlaylist || len(blob.Bytes) != len(cover) {
		t.Fatalf("resolved = %d bytes at level %s, want the %d-byte cover at level playlist",
			len(blob.Bytes), blob.Level, len(cover))
	}
	pl, err := lib.Playlists().Get(ctx, pid)
	if err != nil || !pl.HasArt {
		t.Fatalf("playlist = %+v (err %v), want HasArt after the proxied cover set", pl, err)
	}
}

// TestServeProxiedArtAttribution drives a stamped cover over the socket, which is the
// path an embedder's "enrich now" takes: the source, provider and fetch URL it supplies
// reach the stored mapping instead of arriving as a hand-set cover, and a lock
// instruction of "leave it alone" leaves the pin the entity already carries standing.
func TestServeProxiedArtAttribution(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "catalog.db")
	sock := testsock.Path(t)

	lib := openServedRW(t, ctx, db, t.TempDir(), sock)
	pid, err := lib.Playlists().CreateStatic(ctx, "Mosaic", "", "")
	if err != nil {
		t.Fatalf("create playlist: %v", err)
	}
	c := dialWhenReady(t, sock)

	const url = "https://itunes.example/mosaic.png"
	attr := model.Attribution{Source: model.SourceEnrichment, Provider: "itunes", SourceURL: url}
	if _, err := c.SetEntityArt(ctx, model.ArtPlaylist, pid, model.ArtRoleFront, coverPNG(t), "",
		attr, model.LockOn, false, false); err != nil {
		t.Fatalf("proxied stamped cover: %v", err)
	}
	roles, err := lib.ArtRoles(ctx, model.EntityRef{Type: model.ArtPlaylist, PID: pid})
	if err != nil {
		t.Fatalf("art roles: %v", err)
	}
	if len(roles) != 1 || roles[0].Source != model.SourceEnrichment ||
		roles[0].Provider != "itunes" || roles[0].SourceURL != url || !roles[0].Locked {
		t.Fatalf("front slot = %+v, want a locked itunes cover", roles)
	}

	// A later write that says nothing about the lock leaves it standing, where before it
	// had to read the lock and pass it back with force set.
	if _, err := c.SetEntityArt(ctx, model.ArtPlaylist, pid, model.ArtRoleFront, coverPNG(t), "",
		model.Attribution{}, model.LockUnchanged, true, false); err != nil {
		t.Fatalf("proxied forced cover: %v", err)
	}
	locked, err := lib.ArtLocked(ctx, model.ArtPlaylist, pid, model.ArtRoleFront)
	if err != nil || !locked {
		t.Fatalf("locked after a forced proxied set = %v (err %v), want the lock still standing", locked, err)
	}

	// An unknown lock instruction is refused at the boundary, the way an art role is.
	// The client passes the value through untouched, so this is what a non-Go client
	// sending nonsense looks like.
	if _, err := c.SetEntityArt(ctx, model.ArtPlaylist, pid, model.ArtRoleFront, coverPNG(t), "",
		model.Attribution{}, model.LockChange("yes"), true, false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("unknown wire lock = %v, want CodeInvalid", err)
	}

	// A per-role lock over the socket: the back slot takes a row of its own and gives it
	// back, and the front's own lock is untouched by the round trip. The row is what is
	// read here rather than ArtLocked, which reports the effective lock and so answers
	// true for every role while the front's whole-entity lock stands.
	hasBackLock := func() bool {
		t.Helper()
		roles, err := lib.ArtRoles(ctx, model.EntityRef{Type: model.ArtPlaylist, PID: pid})
		if err != nil {
			t.Fatalf("art roles: %v", err)
		}
		for _, r := range roles {
			if r.Role == model.ArtRoleBack && r.Locked {
				return true
			}
		}
		return false
	}
	if _, err := c.SetArtLock(ctx, model.ArtPlaylist, pid, model.ArtRoleBack, true); err != nil {
		t.Fatalf("proxied back lock: %v", err)
	}
	if !hasBackLock() {
		t.Error("the proxied back lock wrote no locked back slot")
	}
	if _, err := c.SetArtLock(ctx, model.ArtPlaylist, pid, model.ArtRoleBack, false); err != nil {
		t.Fatalf("proxied back unlock: %v", err)
	}
	if hasBackLock() {
		t.Error("the proxied back unlock left the back lock standing")
	}
	if locked, err := lib.ArtLocked(ctx, model.ArtPlaylist, pid, model.ArtRoleFront); err != nil || !locked {
		t.Errorf("front lock after the back round trip = %v (err %v), want it standing", locked, err)
	}
}

// TestServeArtLockRefusesExplicitFrontRole: the front cover has one lock and one home,
// so the wire refuses a role that spells "front" out and points at the omitted form.
// Our own client normalizes, so this is what a non-Go client assembling the frame by
// hand meets; the omitted role still means front and still works.
func TestServeArtLockRefusesExplicitFrontRole(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "catalog.db")
	sock := testsock.Path(t)

	lib := openServedRW(t, ctx, db, t.TempDir(), sock)
	pid, err := lib.Playlists().CreateStatic(ctx, "Pinned", "", "")
	if err != nil {
		t.Fatalf("create playlist: %v", err)
	}
	dialWhenReady(t, sock)

	lockFrame := func(role string) (code, message string) {
		t.Helper()
		params := fmt.Sprintf(`{"entityType":%q,"entityPid":%q,"lock":true}`,
			string(model.ArtPlaylist), string(pid))
		if role != "" {
			params = fmt.Sprintf(`{"entityType":%q,"entityPid":%q,"role":%q,"lock":true}`,
				string(model.ArtPlaylist), string(pid), role)
		}
		return rawFrame(t, sock, fmt.Sprintf(`{"v":%d,"method":%q,"params":%s}`+"\n",
			proxy.ProtocolVersion, proxy.MethodSetArtLock, params))
	}

	code, message := lockFrame("front")
	if code != string(waxerr.CodeInvalid) {
		t.Fatalf("explicit front role on the wire = %q, want CodeInvalid", code)
	}
	if !strings.Contains(message, "omit role") {
		t.Errorf("refusal = %q, want it to point at the omitted form", message)
	}
	if locked, lerr := lib.ArtLocked(ctx, model.ArtPlaylist, pid, model.ArtRoleFront); lerr != nil || locked {
		t.Errorf("the refused call still wrote a lock (%v, err %v)", locked, lerr)
	}
	// An unknown role is refused the same way, and the omitted role still locks the
	// front the way it did before roles existed.
	if code, _ := lockFrame("sleeve"); code != string(waxerr.CodeInvalid) {
		t.Errorf("unknown wire role = %q, want CodeInvalid", code)
	}
	if code, message := lockFrame(""); code != "" {
		t.Fatalf("omitted role = %q/%q, want it to succeed", code, message)
	}
	if locked, lerr := lib.ArtLocked(ctx, model.ArtPlaylist, pid, model.ArtRoleFront); lerr != nil || !locked {
		t.Errorf("front lock after the omitted-role call = %v (err %v), want true", locked, lerr)
	}
}

// rawFrame writes one hand-built request frame to the server socket and returns the
// response's error code and message (both empty on success). It is how a frame no Go
// client would assemble gets tested, the way the version-4 set_star frame above is.
func rawFrame(t *testing.T, sock, frame string) (code, message string) {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("raw dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(frame)); err != nil {
		t.Fatalf("raw write: %v", err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("raw read: %v", err)
	}
	var resp struct {
		OK    bool `json:"ok"`
		Error struct {
			Code string `json:"code"`
			Msg  string `json:"msg"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("unmarshal %q: %v", line, err)
	}
	return resp.Error.Code, resp.Error.Msg
}

// TestServeProxiedArtFormatAndGeneratedSource drives the two v13 additions over the
// socket. A cover whose bytes no decoder here recognizes reaches the store under the
// media type the client fetched it with, instead of being refused as unrecognized, and
// a composed cover reports itself as generated rather than as one a hand chose.
func TestServeProxiedArtFormatAndGeneratedSource(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "catalog.db")
	sock := testsock.Path(t)

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "song.mp3"), testaudio.BuildMP3("Song", "Artist", "Album", 1))
	lib := openServedRW(t, ctx, db, root, sock)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid, err := lib.Playlists().CreateStatic(ctx, "Composed", "", "")
	if err != nil {
		t.Fatalf("create playlist: %v", err)
	}
	c := dialWhenReady(t, sock)

	exotic := append([]byte("\x00exotic"), make([]byte, 60)...)
	if _, err := c.SetEntityArt(ctx, model.ArtPlaylist, pid, model.ArtRoleFront, exotic,
		"image/jxl; charset=binary", model.Attribution{Source: model.SourceGenerated},
		model.LockUnchanged, false, false); err != nil {
		t.Fatalf("proxied generated exotic cover: %v", err)
	}
	roles, err := lib.ArtRoles(ctx, model.EntityRef{Type: model.ArtPlaylist, PID: pid})
	if err != nil {
		t.Fatalf("art roles: %v", err)
	}
	if len(roles) != 1 || roles[0].Format != "jxl" || roles[0].Source != model.SourceGenerated {
		t.Fatalf("front slot = %+v, want a jxl cover sourced generated", roles)
	}

	// Without the hint the same bytes are still refused, so the field is doing the work
	// rather than the refusal having been dropped.
	other := append([]byte("\x00exotic"), make([]byte, 61)...)
	if _, err := c.SetEntityArt(ctx, model.ArtPlaylist, pid, model.ArtRoleBack, other, "",
		model.Attribution{}, model.LockUnchanged, false, false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("unnamed unreadable cover over the wire = %v, want CodeInvalid", err)
	}

	// set_item_art carries the field too, and nothing else observed it reaching the
	// store, so a handler wiring Format to the wrong params field would stay green.
	track := itemPIDByTitle(t, ctx, lib, "Song")
	if _, err := c.SetItemArt(ctx, track, model.ArtRoleBack, append([]byte("\x00exotic"), make([]byte, 62)...),
		"jxl", model.Attribution{}, model.LockUnchanged, false, false); err != nil {
		t.Fatalf("proxied item cover with a format hint: %v", err)
	}
	itemRoles, err := lib.ArtRoles(ctx, model.EntityRef{Type: model.ArtTrack, PID: track})
	if err != nil {
		t.Fatalf("item art roles: %v", err)
	}
	found := false
	for _, r := range itemRoles {
		if r.Role == model.ArtRoleBack {
			found = r.Format == "jxl"
		}
	}
	if !found {
		t.Errorf("item back slot = %+v, want a jxl cover", itemRoles)
	}
}

// TestServeProxiedTranscript drives put_transcript and fetch_transcript over the
// socket: a supplied body is validated and reduced server-side, a fetch error
// keeps its class across the wire, and the stored transcript reads back through
// the server's own library.
func TestServeProxiedTranscript(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	sock := testsock.Path(t)

	lib := openServedRW(t, ctx, db, root, sock)
	show, err := lib.Podcasts().AddManual(ctx, "Proxied", podcast.ManualOptions{})
	if err != nil {
		t.Fatalf("add manual show: %v", err)
	}
	res, err := lib.Podcasts().AddEpisode(ctx, show.PID, model.FeedEpisode{Title: "Ep", GUID: "g1"}, true)
	if err != nil {
		t.Fatalf("add episode: %v", err)
	}
	ep := res.EpisodePID
	c := dialWhenReady(t, sock)

	srt := "1\n00:00:01,000 --> 00:00:04,000\nproxied transcript words\n"
	if err := c.PutTranscript(ctx, ep, "srt", []byte(srt), "https://h/t.srt"); err != nil {
		t.Fatalf("proxied put_transcript: %v", err)
	}
	tr, err := lib.Podcasts().Transcript(ctx, ep)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if tr.Format != "srt" || tr.SourceURL != "https://h/t.srt" {
		t.Fatalf("transcript meta = %+v", tr)
	}
	// The reduction ran server-side: cue timecodes are gone, the words are there.
	if strings.Contains(tr.Body, "-->") || !strings.Contains(tr.Body, "proxied transcript words") {
		t.Fatalf("proxied body not reduced: %q", tr.Body)
	}

	// Validation errors keep their class across the wire.
	if err := c.PutTranscript(ctx, ep, "docx", []byte("x"), ""); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("proxied bad format = %v, want CodeInvalid", err)
	}
	// fetch_transcript on an episode with no declared URL: CodeInvalid, not a
	// transport failure (the fetch would run in the server process).
	if err := c.FetchTranscript(ctx, ep); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("proxied no-url fetch = %v, want CodeInvalid", err)
	}
}

// TestServeProxiedAddRoot drives add_root over the socket: the root lands in the
// server's catalog (the process that scans), a proxied run_scan catalogs a file
// under it, and a validation failure keeps its class across the wire.
func TestServeProxiedAddRoot(t *testing.T) {
	ctx := context.Background()
	rootA := t.TempDir()
	rootB := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	sock := testsock.Path(t)
	writeFile(t, filepath.Join(rootB, "late.mp3"), testaudio.BuildMP3("Proxied Late", "Adder", "Runtime", 1))

	lib := openServedRW(t, ctx, db, rootA, sock)
	c := dialWhenReady(t, sock)

	added, err := c.AddRoot(ctx, proxy.AddRootParams{Path: rootB, Mode: "managed"})
	if err != nil {
		t.Fatalf("proxied add_root: %v", err)
	}
	if added.PID == "" || added.DisplayRoot != rootB || added.Mode != model.ModeManaged {
		t.Fatalf("proxied add_root row = %+v, want a managed row at %s", added, rootB)
	}
	// The server's own library sees it.
	if libs, err := lib.Libraries(ctx); err != nil || len(libs) != 2 {
		t.Fatalf("server libraries = %d (err %v), want 2", len(libs), err)
	}

	// A proxied scan (run in the server process) catalogs the new root's file.
	jobPID, err := c.RunScan(ctx, proxy.ScanParams{})
	if err != nil {
		t.Fatalf("run_scan after add_root: %v", err)
	}
	waitForJobDone(t, ctx, lib, jobPID)
	items, err := lib.Query(ctx, query.New(query.EntityItems).
		Where("title", query.OpIs, "Proxied Late").Build(), "")
	if err != nil || len(items) != 1 {
		t.Fatalf("track under the proxied-added root: err=%v len=%d, want 1", err, len(items))
	}

	// Validation runs server-side and keeps its class on the wire.
	if _, err := c.AddRoot(ctx, proxy.AddRootParams{Path: filepath.Join(rootA, "sub"), Mode: "managed"}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("proxied overlapping add_root = %v, want CodeInvalid", err)
	}
}

// TestServeProxiedDetach drives the detach mutator through the socket while the server
// holds the write lock: the member moves onto its heuristic album and, because the
// write-back flag crossed the wire, its file loses the release tags that would put it
// back on the next rescan.
func TestServeProxiedDetach(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	sock := testsock.Path(t)
	const relMBID = "16161616-1616-1616-1616-161616161616"
	for i, title := range []string{"One", "Two"} {
		writeFile(t, filepath.Join(root, fmt.Sprintf("0%d.mp3", i+1)), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
			Title: title, Artist: "Alpha", AlbumArtist: "Alpha", Album: "Both", Track: i + 1,
			Audio: testaudio.AudioWithSeed(byte(i + 1)),
			TXXX:  []testaudio.TXXXFrame{{Desc: "MusicBrainz Album Id", Value: relMBID}},
		}))
	}

	lib := openServed(t, ctx, db, root, sock)
	pid := itemPIDByTitle(t, ctx, lib, "One")
	c := dialWhenReady(t, sock)

	res, err := c.Detach(ctx, pid, true)
	if err != nil {
		t.Fatalf("proxied detach: %v", err)
	}
	if len(res.WriteBackFailures) != 0 {
		t.Fatalf("unexpected write-back failures: %+v", res.WriteBackFailures)
	}
	if res.OldAlbumPID == "" || res.NewAlbumPID == "" || res.OldAlbumPID == res.NewAlbumPID {
		t.Fatalf("detach result = %+v, want the member on a different album", res)
	}
	after := catalogScalar[string](t, ctx, db,
		`SELECT al.pid FROM album al JOIN track t ON t.album_id = al.id
			JOIN playable_item pi ON pi.id = t.item_id WHERE pi.pid = ?`, string(pid))
	if after != res.NewAlbumPID {
		t.Errorf("catalog album = %q, want the reported %q", after, res.NewAlbumPID)
	}
	fm, err := meta.NewReader().Read(ctx, filepath.Join(root, "01.mp3"))
	if err != nil {
		t.Fatalf("read the detached file: %v", err)
	}
	if fm.Tags.MBReleaseID != "" {
		t.Errorf("detached file still carries release id %q, want the write-back to have stripped it", fm.Tags.MBReleaseID)
	}
}

// TestServeProxiedError checks a domain error keeps its code across the proxy: a
// locked field edited without force returns CodeLocked, so the CLI exit code is the
// same as a local edit.
func TestServeProxiedError(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	sock := testsock.Path(t)
	writeFile(t, filepath.Join(root, "song.mp3"), testaudio.BuildMP3("Original", "Old Artist", "Album", 1))

	lib := openServed(t, ctx, db, root, sock)
	pid := itemPIDByTitle(t, ctx, lib, "Original")
	c := dialWhenReady(t, sock)

	// Lock the field, then a non-force edit of it must be refused with CodeLocked.
	if err := c.Lock(ctx, pid, []string{"artist"}); err != nil {
		t.Fatalf("proxied lock: %v", err)
	}
	_, err := c.EditFields(ctx, pid, map[string]string{"artist": "Nope"}, false,
		model.Attribution{}, model.LockOff, false)
	if !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("edit of a locked field = %v, want CodeLocked", err)
	}
}

// TestMaintenanceHandoffReopen exercises the A6b maintenance-mode cycle: the server
// yields the lock, a foreground process opens the catalog directly and writes, then
// the server reopens and sees the write.
func TestMaintenanceHandoffReopen(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	sock := testsock.Path(t)
	writeFile(t, filepath.Join(root, "song.mp3"), testaudio.BuildMP3("Original", "Old Artist", "Album", 1))

	lib := openServed(t, ctx, db, root, sock)
	c := dialWhenReady(t, sock)

	dvBefore, err := lib.DataVersion(ctx)
	if err != nil {
		t.Fatalf("data version: %v", err)
	}

	// Hand off: the server closes and releases the lock.
	if err := c.MaintenanceBegin(ctx); err != nil {
		t.Fatalf("maintenance begin: %v", err)
	}

	// A direct read-write open now succeeds, proving the lock was released.
	lib2, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: db,
		Roots:  []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
	})
	if err != nil {
		t.Fatalf("direct open during maintenance should succeed: %v", err)
	}
	if _, err := lib2.CreateUser(ctx, "ViaMaintenance"); err != nil {
		_ = lib2.Close()
		t.Fatalf("foreground mutation: %v", err)
	}
	if err := lib2.Close(); err != nil { // release the lock
		t.Fatalf("close foreground lib: %v", err)
	}

	// End maintenance: the server reopens and reacquires the lock.
	if err := c.MaintenanceEnd(ctx); err != nil {
		t.Fatalf("maintenance end: %v", err)
	}

	// The reopened server sees the foreground write.
	users, err := lib.Users(ctx)
	if err != nil {
		t.Fatalf("users after reopen: %v", err)
	}
	if !hasUsernamed(users, "ViaMaintenance") {
		t.Fatalf("users after reopen = %+v, want ViaMaintenance", users)
	}
	// DataVersion advanced across the hand-off.
	if dvAfter, err := lib.DataVersion(ctx); err != nil || dvAfter == dvBefore {
		t.Fatalf("data version after reopen = %d (err %v), want != %d", dvAfter, err, dvBefore)
	}
	// The server can mutate again through the proxy.
	if _, err := c.CreateUser(ctx, "AfterReopen"); err != nil {
		t.Fatalf("proxied mutation after reopen: %v", err)
	}
	if users, _ := lib.Users(ctx); !hasUsernamed(users, "AfterReopen") {
		t.Fatalf("users = %+v, want AfterReopen", users)
	}
}

// TestServerRunJobKeepsServerUp covers the core of the A6a design. A long job
// (scan/analyze/enrich/organize) submitted to a running server runs in the server's
// process, so the server never closes and stays available. The maintenance hand-off
// would pause it instead. A CLI-embedding host such as WaxDeck must keep serving
// while a submitted scan runs.
func TestServerRunJobKeepsServerUp(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	sock := testsock.Path(t)
	writeFile(t, filepath.Join(root, "a.mp3"), testaudio.BuildMP3WithAudio("A", "Artist", "Album", 1, testaudio.AudioWithSeed(1)))
	writeFile(t, filepath.Join(root, "b.mp3"), testaudio.BuildMP3WithAudio("B", "Artist", "Album", 2, testaudio.AudioWithSeed(2)))
	writeFile(t, filepath.Join(root, "c.mp3"), testaudio.BuildMP3WithAudio("C", "Artist", "Album", 3, testaudio.AudioWithSeed(3)))

	lib := openServedRW(t, ctx, db, root, sock) // no initial scan; the server-run job does it
	c := dialWhenReady(t, sock)

	// Submit the scan as a server-run job; it returns a job PID immediately.
	jobPID, err := c.RunScan(ctx, proxy.ScanParams{})
	if err != nil {
		t.Fatalf("run_scan: %v", err)
	}
	if jobPID == "" {
		t.Fatal("run_scan returned an empty job pid")
	}

	// The server was NOT paused: a proxied fast mutation still succeeds (it would be
	// a CodeConflict "in maintenance" on the hand-off path), and the server's own
	// library handle is still open (it would error "store is closed" if maintenance
	// had closed it). This is the property the correction restores.
	if _, err := c.CreateUser(ctx, "DuringJob"); err != nil {
		t.Fatalf("proxied mutation while a server-run job runs: %v", err)
	}
	if _, err := lib.Users(ctx); err != nil {
		t.Fatalf("server library was closed during a server-run job: %v", err)
	}

	// Tail the job to completion through the (still-open) catalog.
	job := waitForJobDone(t, ctx, lib, jobPID)

	// The job actually ran the scan and recorded its result summary for the tailer.
	var res scan.Result
	if err := json.Unmarshal([]byte(job.Result), &res); err != nil {
		t.Fatalf("decode job result %q: %v", job.Result, err)
	}
	if res.ItemsCreated != 3 {
		t.Fatalf("scan result created = %d, want 3 (result=%q)", res.ItemsCreated, job.Result)
	}
	// The catalog reflects both the job's work and the concurrent proxied mutation.
	if n, err := lib.Count(ctx, query.New(query.EntityItems).Build(), ""); err != nil || n != 3 {
		t.Fatalf("catalog item count = %d (err %v), want 3", n, err)
	}
	if users, _ := lib.Users(ctx); !hasUsernamed(users, "DuringJob") {
		t.Fatalf("users = %+v, want DuringJob (the mutation served during the job)", users)
	}
}

// waitForJobDone polls a job until it reaches a terminal state and fails the test
// unless it finished successfully.
func waitForJobDone(t *testing.T, ctx context.Context, lib *waxbin.Library, jobPID model.PID) *model.Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		job, err := lib.Job(ctx, jobPID)
		if err != nil {
			t.Fatalf("job read: %v", err)
		}
		switch job.State {
		case model.JobDone:
			return job
		case model.JobFailed, model.JobCrashed, model.JobCanceled:
			t.Fatalf("job ended %s: %s", job.State, job.Error)
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("job %s did not finish", jobPID)
	return nil
}

// TestMaintenanceRefusedWhileJobRuns verifies a maintenance hand-off is refused
// while a server-run job is in flight, rather than closing the store out from under
// the running scan (which would abort it partway). See BeginMaintenance's guard.
func TestMaintenanceRefusedWhileJobRuns(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	// Enough distinct files that the scan is unmistakably still running when we check
	// (StartScan returns the moment the job row exists, before the work processes any
	// file, so the scan has all of these still to do).
	for i := 0; i < 24; i++ {
		writeFile(t, filepath.Join(root, fmt.Sprintf("t%02d.mp3", i)),
			testaudio.BuildMP3WithAudio(fmt.Sprintf("S%d", i), "Artist", "Album", i+1, testaudio.AudioWithSeed(byte(i+1))))
	}
	lib := openManaged(t, ctx, db, root)

	jobPID, err := lib.StartScan(ctx, waxbin.ScanRequest{})
	if err != nil {
		t.Fatalf("start scan: %v", err)
	}
	if jobPID == "" {
		t.Fatal("empty job pid")
	}

	// A hand-off must be refused with CodeConflict while the job runs.
	if err := lib.BeginMaintenance(ctx); !waxerr.Is(err, waxerr.CodeConflict) {
		t.Fatalf("BeginMaintenance while a job runs = %v, want CodeConflict", err)
	}

	// The scan was not disturbed: it completes normally.
	job := waitForJobDone(t, ctx, lib, jobPID)
	var res scan.Result
	if err := json.Unmarshal([]byte(job.Result), &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if res.ItemsCreated != 24 {
		t.Fatalf("scan created = %d, want 24 (the refused hand-off must not abort it)", res.ItemsCreated)
	}
}

// TestSubscriberSurvivesMaintenance verifies an in-process change subscription
// survives a maintenance hand-off: BeginMaintenance suspends the store (keeping
// subscribers) rather than closing it, so after EndMaintenance the embedder's
// channel still delivers deltas for subsequent writes.
func TestSubscriberSurvivesMaintenance(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	sock := testsock.Path(t)
	writeFile(t, filepath.Join(root, "song.mp3"), testaudio.BuildMP3("Original", "Old Artist", "Album", 1))

	lib := openServed(t, ctx, db, root, sock)
	pid := itemPIDByTitle(t, ctx, lib, "Original")

	ch, cancel := lib.Subscribe()
	defer cancel()

	c := dialWhenReady(t, sock)
	if err := c.MaintenanceBegin(ctx); err != nil {
		t.Fatalf("maintenance begin: %v", err)
	}
	lib2, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: db,
		Roots:  []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
	})
	if err != nil {
		t.Fatalf("foreground open: %v", err)
	}
	if _, err := lib2.CreateUser(ctx, "fg"); err != nil {
		_ = lib2.Close()
		t.Fatalf("foreground mutation: %v", err)
	}
	_ = lib2.Close()
	if err := c.MaintenanceEnd(ctx); err != nil {
		t.Fatalf("maintenance end: %v", err)
	}

	// A write through the reopened server library must still publish to the
	// subscription (the channel was not closed by the hand-off).
	if err := lib.EditField(ctx, pid, "genre", "PostMaint", waxbin.EditOptions{Lock: model.LockOn}); err != nil {
		t.Fatalf("post-maintenance edit: %v", err)
	}
	select {
	case _, ok := <-ch:
		if !ok {
			t.Fatal("subscription channel was closed by the maintenance hand-off")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no change delivered after maintenance; the subscription was lost")
	}
}

func hasUser(users []*model.User, pid model.PID, name string) bool {
	for _, u := range users {
		if u.PID == pid && u.Name == name {
			return true
		}
	}
	return false
}

func hasUsernamed(users []*model.User, name string) bool {
	for _, u := range users {
		if u.Name == name {
			return true
		}
	}
	return false
}

// TestServeProxiedPodcastUnfetchAndRemove drives the two v9 methods end to end. Both
// used to take the maintenance hand-off, which paused the whole server for what is a
// short leased mutation; over the socket the server stays up and the episode still
// comes back remote rather than archived.
func TestServeProxiedPodcastUnfetchAndRemove(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	podDir := t.TempDir()
	acq := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	sock := testsock.Path(t)

	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath:    db,
		Roots:     []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
		Podcasts:  config.PodcastConfig{Dir: podDir},
		IPCSocket: sock,
	})
	if err != nil {
		t.Fatalf("open served library: %v", err)
	}
	serveLib(t, ctx, lib, sock)
	c := dialWhenReady(t, sock)

	epFile := filepath.Join(acq, "ep.mp3")
	writeFile(t, epFile, testaudio.BuildMP3WithAudio("Ep One", "Host", "Show", 1, testaudio.AudioWithSeed(5)))
	res, err := lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: epFile}, model.KindEpisode, waxbin.AcquiredMeta{
		ShowTitle: "Served Show", SourceType: model.SourceManual, Title: "Ep One",
	})
	if err != nil {
		t.Fatalf("import episode: %v", err)
	}
	before, err := lib.Podcasts().Episode(ctx, res.EpisodePID)
	if err != nil {
		t.Fatalf("Episode: %v", err)
	}
	path := before.Episode.DisplayPath

	got, err := c.Unfetch(ctx, res.EpisodePID)
	if err != nil {
		t.Fatalf("proxied unfetch: %v", err)
	}
	if !got.Unfetched || got.ReclaimedBytes <= 0 {
		t.Errorf("proxied unfetch = %+v, want the bytes reclaimed", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the episode file survives at %s", path)
	}
	after, err := lib.Podcasts().Episode(ctx, res.EpisodePID)
	if err != nil {
		t.Fatalf("Episode after: %v", err)
	}
	if after.Episode.Downloaded || after.Episode.State != model.StateRemote {
		t.Errorf("episode = %+v, want remote and not downloaded", after.Episode)
	}
	// A second unfetch is a no-op, not an error, and the flag is what says so.
	again, err := c.Unfetch(ctx, res.EpisodePID)
	if err != nil {
		t.Fatalf("second proxied unfetch: %v", err)
	}
	if again.Unfetched || again.ReclaimedBytes != 0 {
		t.Errorf("second proxied unfetch = %+v, want a no-op", again)
	}
	// An unknown episode keeps its class across the wire.
	if _, err := c.Unfetch(ctx, model.PID("no-such-episode")); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("proxied unfetch of an unknown episode = %v, want CodeNotFound", err)
	}

	if err := c.PodcastRemove(ctx, before.Episode.PodcastPID); err != nil {
		t.Fatalf("proxied podcast remove: %v", err)
	}
	if _, err := lib.Podcasts().Get(ctx, before.Episode.PodcastPID); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("show after proxied remove = %v, want CodeNotFound", err)
	}
	if err := c.PodcastRemove(ctx, before.Episode.PodcastPID); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("proxied remove of an already-removed show = %v, want CodeNotFound", err)
	}
}

// TestServeProxiedPlaylistLifecycle drives the four playlist lifecycle methods
// over the socket. Before they existed these commands took the maintenance
// hand-off, which stopped the server for the length of a single row write, so
// what this test really pins is that creating, renaming, importing and deleting
// a playlist no longer needs the server to stand down.
func TestServeProxiedPlaylistLifecycle(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	sock := testsock.Path(t)
	song := filepath.Join(root, "song.mp3")
	writeFile(t, song, testaudio.BuildMP3("Original", "Old Artist", "Album", 1))

	lib := openServed(t, ctx, db, root, sock)
	c := dialWhenReady(t, sock)

	// Static create: the pid comes back over the wire, and the server that holds
	// the lock is the one that wrote the row.
	pid, err := c.PlaylistCreate(ctx, "Mix", "", "", nil)
	if err != nil {
		t.Fatalf("proxied create: %v", err)
	}
	pl, err := lib.Playlists().Get(ctx, pid)
	if err != nil || pl.Name != "Mix" || pl.Kind != model.PlaylistStatic {
		t.Fatalf("created playlist = %+v (err %v), want a static Mix", pl, err)
	}

	if err := c.PlaylistRename(ctx, pid, "Renamed"); err != nil {
		t.Fatalf("proxied rename: %v", err)
	}
	if pl, err := lib.Playlists().Get(ctx, pid); err != nil || pl.Name != "Renamed" {
		t.Fatalf("name after proxied rename = %+v (err %v), want Renamed", pl, err)
	}

	// Smart create: the rule travels as a marshaled document and is evaluated by
	// the server, so membership follows on the next read.
	rule := query.New(query.EntityItems).Where("title", query.OpContains, "Origin").Build()
	doc, err := query.MarshalRule(rule)
	if err != nil {
		t.Fatalf("marshal rule: %v", err)
	}
	smart, err := c.PlaylistCreate(ctx, "Smart", "", "", doc)
	if err != nil {
		t.Fatalf("proxied smart create: %v", err)
	}
	items, err := lib.Playlists().Items(ctx, smart, "")
	if err != nil || len(items) != 1 || items[0].Title != "Original" {
		t.Fatalf("smart membership = %v (err %v), want [Original]", items, err)
	}

	// A rule the store rejects keeps its class across the wire, and no playlist is
	// left behind.
	bad, err := query.MarshalRule(query.New(query.EntityItems).Where("bogus", query.OpIs, "x").Build())
	if err != nil {
		t.Fatalf("marshal bad rule: %v", err)
	}
	if _, err := c.PlaylistCreate(ctx, "Bad", "", "", bad); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("proxied bad rule err = %v, want CodeInvalid", err)
	}

	// Import: the document travels whole and the path matching happens on the
	// server, which is the side that has the catalog. The unmatched entry is
	// reported rather than invented.
	m3u := "#EXTM3U\n" + song + "\n" + filepath.Join(root, "missing.mp3") + "\n"
	res, err := c.PlaylistImportM3U8(ctx, "Imported", "", "", []byte(m3u))
	if err != nil {
		t.Fatalf("proxied import: %v", err)
	}
	if res.Matched != 1 || res.Unmatched != 1 || len(res.UnmatchedPaths) != 1 {
		t.Fatalf("import result = %+v, want 1 matched and 1 unmatched", res)
	}
	if items, err := lib.Playlists().Items(ctx, model.PID(res.PlaylistPID), ""); err != nil || len(items) != 1 {
		t.Fatalf("imported membership = %v (err %v), want the one matched track", items, err)
	}

	if err := c.PlaylistDelete(ctx, pid); err != nil {
		t.Fatalf("proxied delete: %v", err)
	}
	if _, err := lib.Playlists().Get(ctx, pid); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("get after proxied delete = %v, want CodeNotFound", err)
	}
}

// TestServeProxiedRenameEntity checks the rename params reach the handler and the report
// comes back over the wire, including the outcome and member count a client reads to know
// which branch ran.
func TestServeProxiedRenameEntity(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	sock := testsock.Path(t)
	for i, title := range []string{"One", "Two"} {
		writeFile(t, filepath.Join(root, fmt.Sprintf("0%d.mp3", i+1)), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
			Title: title, Artist: "Alpha", AlbumArtist: "Alpha", Album: "Old Title", Track: i + 1,
			Audio: testaudio.AudioWithSeed(byte(i + 1)),
		}))
	}
	openServed(t, ctx, db, root, sock)
	albumPID := model.PID(catalogScalar[string](t, ctx, db, "SELECT pid FROM album"))
	c := dialWhenReady(t, sock)

	res, err := c.RenameEntity(ctx, model.MergeAlbum, albumPID,
		map[string]string{"album": "New Title"}, true, model.Attribution{}, model.LockUnchanged, false)
	if err != nil {
		t.Fatalf("proxied rename: %v", err)
	}
	if len(res.WriteBackFailures) != 0 {
		t.Fatalf("unexpected write-back failures: %+v", res.WriteBackFailures)
	}
	if res.Outcome != string(model.EntityRenamed) || res.Members != 2 {
		t.Fatalf("rename result = %+v, want a two-member rename in place", res)
	}
	if title := catalogScalar[string](t, ctx, db, "SELECT title FROM album WHERE pid = ?", string(albumPID)); title != "New Title" {
		t.Errorf("catalog album title = %q, want the renamed one", title)
	}
	fm, err := meta.NewReader().Read(ctx, filepath.Join(root, "01.mp3"))
	if err != nil {
		t.Fatalf("read the renamed file: %v", err)
	}
	if fm.Tags.Album != "New Title" {
		t.Errorf("file ALBUM = %q, want the write-back to have followed", fm.Tags.Album)
	}
}

// TestServeAcquisitionCuration runs both acquisition verbs end to end over the socket:
// the correction, the clear's default lock, and the CodeLocked refusal surviving the
// wire so the CLI's exit code matches a local run.
func TestServeAcquisitionCuration(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	sock := testsock.Path(t)
	src := filepath.Join(root, "song.mp3")
	writeFile(t, src, testaudio.BuildMP3("Ripped", "Artist", "Album", 1))
	if _, err := meta.NewWriter().Apply(ctx, src, []meta.TagEdit{
		{Key: "SOURCE_URL", Values: []string{"https://wrong.test/x"}},
		{Key: "SOURCE_ID", Values: []string{"wrong-1"}},
	}); err != nil {
		t.Fatalf("stamp acquisition tags: %v", err)
	}

	lib := openServed(t, ctx, db, root, sock)
	pid := itemPIDByTitle(t, ctx, lib, "Ripped")
	c := dialWhenReady(t, sock)

	if a, err := lib.Acquisition(ctx, pid); err != nil || a.SourceURL != "https://wrong.test/x" {
		t.Fatalf("scan did not derive the wrong origin first: %+v (err %v)", a, err)
	}
	if _, err := c.SetAcquisition(ctx, pid, model.AcquisitionInput{
		SourceType: model.SourceManual, SourceURL: "https://right.test/y",
	}, model.LockOn, false, false); err != nil {
		t.Fatalf("proxied set_acquisition: %v", err)
	}
	a, err := lib.Acquisition(ctx, pid)
	if err != nil {
		t.Fatalf("Acquisition: %v", err)
	}
	if a.SourceType != model.SourceManual || a.SourceURL != "https://right.test/y" || a.SourceID != "" {
		t.Errorf("proxied set = %+v, want the authoritative replace", a)
	}

	// The lock came back over the wire, so an unforced correction is refused with the
	// same code a local one would give.
	if _, err := c.SetAcquisition(ctx, pid, model.AcquisitionInput{SourceType: model.SourceRSS},
		model.LockUnchanged, false, false); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("proxied set over a lock = %v, want CodeLocked", err)
	}
	if _, err := c.ClearAcquisition(ctx, pid, model.LockUnchanged, false, false); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("proxied clear over a lock = %v, want CodeLocked", err)
	}

	if _, err := c.ClearAcquisition(ctx, pid, model.LockUnchanged, true, false); err != nil {
		t.Fatalf("proxied forced clear: %v", err)
	}
	if _, err := lib.Acquisition(ctx, pid); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("proxied clear did not remove the row: %v", err)
	}
	// The clear's default lock reached the store, which is what keeps it across a scan.
	rows, err := lib.Provenance(ctx, pid)
	if err != nil {
		t.Fatalf("Provenance: %v", err)
	}
	var locked bool
	for _, r := range rows {
		if r.Field == "acquisition" && r.Locked {
			locked = true
		}
	}
	if !locked {
		t.Error("a proxied bare clear left the acquisition field unlocked")
	}
}
