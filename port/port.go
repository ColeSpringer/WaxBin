// Package port handles catalog backup, restore, redaction, and logical JSON
// export. A byte backup is the disaster-recovery artifact because it preserves
// every table; the logical export is for inspection and cross-tool portability
// and never includes secrets.
package port

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/colespringer/waxbin/waxerr"
	_ "modernc.org/sqlite"
)

// ExportFormat tags the logical export so a reader can recognize it.
const ExportFormat = "waxbin-export"

// ExportVersion is the logical export schema version, independent of the storage
// schema. It rises when the export shape changes. Version 2 encodes
// unix-nanosecond timestamps as decimal strings (",string" fields): the values
// exceed IEEE-754 double precision, so the version-1 bare numbers were
// corrupted by any consumer parsing JSON numbers into doubles. A version-1
// file no longer parses (the typed decode rejects the unquoted numbers),
// accepted pre-release. Version 3 adds the item's MusicBrainz id and ISRC and is
// purely additive: it parses identically under any consumer that handled version 2.
// Version 4 adds the played/finished change stamp and is additive the same way.
// Version 5 adds a track's bpm, additive again.
const ExportVersion = 5

// Manifest is the versioned header of a logical export.
type Manifest struct {
	Format        string `json:"format"`
	Version       int    `json:"version"`
	CreatedAt     int64  `json:"createdAt,string"` // unix nanoseconds, as a decimal string
	SchemaVersion int    `json:"schemaVersion"`
	Items         int    `json:"items"`
	Libraries     int    `json:"libraries"`
	PlayStates    int    `json:"playStates"`
}

// Snapshot is a logical export: catalog metadata plus per-user playback state.
// It never contains secrets.
type Snapshot struct {
	Manifest  Manifest          `json:"manifest"`
	Libraries []LibraryExport   `json:"libraries"`
	Items     []ItemExport      `json:"items"`
	PlayState []PlayStateExport `json:"playState"`
}

// LibraryExport is a registered root in an export.
type LibraryExport struct {
	PID     string `json:"pid"`
	Root    string `json:"root"`
	Mode    string `json:"mode"`
	Profile string `json:"profile"`
}

// ItemExport is one item's portable metadata.
type ItemExport struct {
	PID         string `json:"pid"`
	Kind        string `json:"kind"`
	State       string `json:"state"`
	Title       string `json:"title"`
	Artist      string `json:"artist,omitempty"`
	AlbumArtist string `json:"albumArtist,omitempty"`
	Album       string `json:"album,omitempty"`
	TrackNo     int    `json:"trackNo,omitempty"`
	DiscNo      int    `json:"discNo,omitempty"`
	Year        int    `json:"year,omitempty"`
	Genre       string `json:"genre,omitempty"`
	BPM         int    `json:"bpm,omitempty"`
	RelPath     string `json:"relPath,omitempty"`
	// The item's own external ids, the strongest cross-tool keys there are. Only its
	// own two: this is a flat per-item record carrying no relational handles, and an
	// album or artist id here would denormalize entity identity into every row.
	MBID string `json:"mbid,omitempty"`
	ISRC string `json:"isrc,omitempty"`

	// Artist is the combined display string, and the track's split artist credit is
	// deliberately NOT carried alongside it. The credit is a relational fan-out (one
	// item_contributor row per artist), which is the thing this record does not carry,
	// and a consumer rebuilding a catalog from a snapshot re-derives it from Artist the
	// same way a scan does.
	//
	// That re-derive is lossy in one case worth naming: a credit the user curated
	// stores as a comma-joined display, and the splitter does not split on a comma, so
	// a rebuilt catalog would collapse it back to one artist. Carrying the list would
	// need an importer that writes contributor rows, and there is none: Restore below
	// restores a physical backup, not a snapshot.
}

// PlayStateExport is one user's critical state for one item. The changed-at
// stamps (unix nanoseconds, 0 = never changed) record when the star, rating, or
// played/finished pair last changed value, so a consumer replaying an export can
// order it against state it already holds. They are JSON-encoded as decimal
// strings (",string"): ns epochs exceed IEEE-754 double precision, and an
// ordering token corrupted by a double-based parser would misorder silently.
type PlayStateExport struct {
	UserPID          string `json:"userPid"`
	ItemPID          string `json:"itemPid"`
	PositionMS       int64  `json:"positionMs,omitempty"`
	Played           bool   `json:"played,omitempty"`
	Finished         bool   `json:"finished,omitempty"`
	PlayCount        int    `json:"playCount,omitempty"`
	Rating           *int   `json:"rating,omitempty"`
	Starred          bool   `json:"starred,omitempty"`
	RatingChangedNS  int64  `json:"ratingChangedNs,string,omitempty"`
	StarredChangedNS int64  `json:"starredChangedNs,string,omitempty"`
	PlayedChangedNS  int64  `json:"playedChangedNs,string,omitempty"`
}

// BuildSnapshot assembles a logical export from already-read data. relPathOf maps
// an item pid to its primary file's rel path (empty if none); pass nil to omit.
func BuildSnapshot(schemaVersion int, createdAt int64, libs []*model.Library, items []*model.ItemView, plays []model.PlayState, relPathOf func(model.PID) string) *Snapshot {
	snap := &Snapshot{}
	for _, l := range libs {
		root := l.DisplayRoot
		if root == "" {
			root = string(l.Root)
		}
		snap.Libraries = append(snap.Libraries, LibraryExport{
			PID: string(l.PID), Root: root, Mode: string(l.Mode), Profile: l.Profile,
		})
	}
	for _, it := range items {
		ie := ItemExport{
			PID: string(it.PID), Kind: string(it.Kind), State: string(it.State), Title: it.Title,
			Artist: it.Artist, AlbumArtist: it.AlbumArtist, Album: it.Album,
			TrackNo: it.TrackNo, DiscNo: it.DiscNo, Year: it.Year, Genre: it.Genre,
			BPM: it.BPM, MBID: it.MBID, ISRC: it.ISRC,
		}
		if relPathOf != nil {
			ie.RelPath = relPathOf(it.PID)
		}
		snap.Items = append(snap.Items, ie)
	}
	for _, ps := range plays {
		pe := PlayStateExport{
			UserPID: string(ps.UserPID), ItemPID: string(ps.ItemPID), PositionMS: ps.PositionMS,
			Played: ps.Played, Finished: ps.Finished, PlayCount: ps.PlayCount, Starred: ps.Starred,
			RatingChangedNS: ps.RatingChangedAt, StarredChangedNS: ps.StarredChangedAt,
			PlayedChangedNS: ps.PlayedChangedAt,
		}
		if ps.HasRating {
			r := ps.Rating
			pe.Rating = &r
		}
		snap.PlayState = append(snap.PlayState, pe)
	}
	snap.Manifest = Manifest{
		Format: ExportFormat, Version: ExportVersion, CreatedAt: createdAt,
		SchemaVersion: schemaVersion, Items: len(snap.Items),
		Libraries: len(snap.Libraries), PlayStates: len(snap.PlayState),
	}
	return snap
}

// WriteSnapshot writes a snapshot as indented JSON.
func WriteSnapshot(w io.Writer, snap *Snapshot) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(snap); err != nil {
		return waxerr.Wrap(waxerr.CodeInternal, "port.WriteSnapshot", err)
	}
	return nil
}

// ReadSnapshot parses a logical export, rejecting an unrecognized format.
func ReadSnapshot(r io.Reader) (*Snapshot, error) {
	var snap Snapshot
	if err := json.NewDecoder(r).Decode(&snap); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeInvalid, "port.ReadSnapshot", err)
	}
	if snap.Manifest.Format != ExportFormat {
		return nil, waxerr.New(waxerr.CodeInvalid, "port.ReadSnapshot", "not a WaxBin export")
	}
	return &snap, nil
}

// RedactBackupFile strips the secret table from a backup copy and VACUUMs so the
// removed bytes do not linger in free pages. A backup from a schema without the
// secret table is a clean no-op.
func RedactBackupFile(ctx context.Context, path string) error {
	const op = "port.RedactBackupFile"
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "DELETE FROM secret"); err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil
		}
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if _, err := db.ExecContext(ctx, "VACUUM"); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return nil
}

// ValidateBackup opens a backup read-only and returns its recorded schema
// version, confirming it is a WaxBin catalog built from the same schema baseline
// as this build.
//
// The baseline check is not about protecting the target from a partial copy, which
// Restore's temp-and-rename already covers. It closes the opposite risk: a fully
// successful restore of an incompatible backup leaves a catalog the store refuses
// to open.
func ValidateBackup(ctx context.Context, path string) (int, error) {
	const op = "port.ValidateBackup"
	if _, err := os.Stat(path); err != nil {
		return 0, waxerr.Wrapf(waxerr.CodeNotFound, op, err, "opening backup %s", path)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer db.Close()
	// schema_migrations first, so a file that is not a WaxBin catalog at all says so
	// rather than reporting a baseline it was never going to match.
	var v int
	err = db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version),0) FROM schema_migrations").Scan(&v)
	if err != nil {
		return 0, waxerr.Wrapf(waxerr.CodeInvalid, op, err, "not a WaxBin catalog")
	}
	want, err := sqlite.BaselineFingerprint()
	if err != nil {
		return 0, err
	}
	var stamp int64
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&stamp); err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if int32(stamp) != want {
		return 0, waxerr.New(waxerr.CodeUnsupported, op, fmt.Sprintf(
			"backup was built from a different schema baseline (backup %08x, this build %08x); "+
				"restoring it would leave a catalog this build refuses to open. Read what it still "+
				"holds with `waxbin --db %s --read-only --allow-stale ...` instead",
			uint32(int32(stamp)), uint32(want), path))
	}
	return v, nil
}

// Census counts what a catalog file holds, so `db reset` can say what it is about
// to discard.
type Census struct {
	Items        int
	PlayStates   int
	Playlists    int
	Podcasts     int
	TrashEntries int
	// Roots is every user-registered display_root, including ones added at runtime
	// that configuration will not bring back. The internal podcast library is
	// excluded: it is re-created on demand and is not a root anyone can re-add.
	Roots []string
	// Partial is set when a table could not be read, so the counts understate what
	// the catalog holds. A hot -wal after a crash fails every read on a mode=ro
	// connection, and reporting those zeros as the contents would tell the operator
	// a full catalog is empty.
	Partial bool
}

// ReadCensus opens a catalog file read-only and counts what it holds. It reads only
// counts and display_root, never a column an in-place baseline edit may have added,
// since the catalog is one the store itself will not open. A file too damaged to
// census must still be resettable, so a failed read sets Partial rather than
// failing the call.
func ReadCensus(ctx context.Context, path string) (*Census, error) {
	const op = "port.ReadCensus"
	if _, err := os.Stat(path); err != nil {
		return nil, waxerr.Wrapf(waxerr.CodeNotFound, op, err, "opening catalog %s", path)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer db.Close()

	c := &Census{}
	for _, t := range []struct {
		query string
		into  *int
	}{
		{"SELECT COUNT(*) FROM playable_item", &c.Items},
		{"SELECT COUNT(*) FROM play_state", &c.PlayStates},
		{"SELECT COUNT(*) FROM playlist", &c.Playlists},
		{"SELECT COUNT(*) FROM podcast", &c.Podcasts},
		// Restorable entries only, matching what `trash list` shows: a restored row
		// is journal history and nothing is at stake in discarding it.
		{"SELECT COUNT(*) FROM trash WHERE restored_at IS NULL", &c.TrashEntries},
	} {
		var n int
		if err := db.QueryRowContext(ctx, t.query).Scan(&n); err != nil {
			c.Partial = true
			continue
		}
		*t.into = n
	}
	rows, err := db.QueryContext(ctx,
		"SELECT display_root FROM library WHERE mode != ? ORDER BY display_root", string(model.ModePodcast))
	if err != nil {
		c.Partial = true
		return c, nil
	}
	defer rows.Close()
	for rows.Next() {
		var root string
		if err := rows.Scan(&root); err != nil {
			c.Partial = true
			return c, nil
		}
		c.Roots = append(c.Roots, root)
	}
	return c, nil
}

// Restore byte-copies a validated backup over the target catalog path. It removes
// the target's stale WAL/shm sidecars so the restored file is authoritative, and
// refuses to overwrite an existing catalog unless force is set. The caller must
// ensure no process has the target open.
func Restore(ctx context.Context, backupPath, targetPath string, force bool) error {
	const op = "port.Restore"
	if _, err := ValidateBackup(ctx, backupPath); err != nil {
		return err
	}
	if _, err := os.Stat(targetPath); err == nil && !force {
		return waxerr.New(waxerr.CodeConflict, op, "target catalog exists (pass force to overwrite): "+targetPath)
	} else if err != nil && !os.IsNotExist(err) {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	// Copy to a sibling temp file and atomically rename it into place, so an
	// interrupted copy (out of space, cancellation, I/O error) leaves the existing
	// catalog untouched instead of truncating it. The temp lives in the target's
	// directory so the rename stays on one filesystem (and thus atomic).
	tmp := targetPath + ".restore-tmp"
	if err := copyFile(ctx, backupPath, tmp); err != nil {
		return err // copyFile removes its own partial temp on failure
	}
	// Only now that a complete copy exists do we disturb the target: drop its stale
	// WAL/shm (which would otherwise shadow the restored file) and swap it in.
	_ = os.Remove(targetPath + "-wal")
	_ = os.Remove(targetPath + "-shm")
	if err := os.Rename(tmp, targetPath); err != nil {
		_ = os.Remove(tmp)
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	// The restored catalog carries the secret table, so restrict it to owner-only.
	// This is best-effort: a filesystem without Unix permissions is not a restore
	// failure, and the next read-write open re-applies it anyway.
	_ = os.Chmod(targetPath, 0o600)
	return nil
}

// copyFile copies src to a freshly created dst, honoring context cancellation, and
// removes the partial dst on any failure. The caller is expected to pass a temp
// path so a failure never damages a real target.
func copyFile(ctx context.Context, src, dst string) error {
	const op = "port.copy"
	in, err := os.Open(src)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = out.Close()
			_ = os.Remove(dst)
		}
	}()

	// Copy in bounded chunks, checking the context between them, so a canceled
	// restore stops promptly instead of blocking until EOF on a multi-gigabyte file.
	buf := make([]byte, 1<<20) // 1 MiB
	for {
		if err := ctx.Err(); err != nil {
			return waxerr.FromContext(op, err, waxerr.CodeIO)
		}
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, werr)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, rerr)
		}
	}
	if err := out.Sync(); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if err := out.Close(); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	ok = true
	return nil
}
