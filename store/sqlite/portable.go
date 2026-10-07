package sqlite

import (
	"context"
	"database/sql"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// This file holds the read primitives behind the cross-catalog sharing feature
// (facade ResolveRefWith/ExportPlaylistRefs). Every method is a pure read on the read
// pool (s.read): it writes no change_log rows and is safe on a read-only Library. None of
// them belongs to the model.Catalog port. They are extra methods on *Store that the
// facade calls directly, so the port stays as it is and the var _ model.Catalog assertion
// still holds.

// ItemsByEssence returns every item backed by a file with the given essence hash,
// matching ANY of an item's files rather than only its primary edge. Matching any file
// means a multi-file audiobook resolves from any of its parts, and it stays consistent
// with the inbox duplicate gate (FileByEssence is also any-file). The result is
// 0, 1, or N items: a single-file rip whose bytes were shared to another catalog is
// one; a single-file CUE album is N (each virtual track has its own primary edge to the
// one shared file), which the caller disambiguates by kind then duration. Returns an
// empty slice, not an error, on a clean miss.
func (s *Store) ItemsByEssence(ctx context.Context, essence string) ([]*model.ItemView, error) {
	const op = "store.ItemsByEssence"
	if essence == "" {
		return nil, nil
	}
	rows, err := s.read.QueryContext(ctx, itemSelect+` WHERE pi.id IN (
		SELECT itf.item_id FROM item_file itf
		JOIN file f2 ON f2.id = itf.file_id
		WHERE f2.essence_hash = ?)`, essence)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []*model.ItemView
	for rows.Next() {
		v, err := scanItemView(rows)
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return out, nil
}

// ItemsByContentHash returns every item backed by a file with the given content
// hash, matching any of an item's files, mirroring ItemsByEssence over the
// file_content index. The content hash covers the whole file byte for byte
// (identity.ContentHash, "sha256:" plus hex), so it changes on any tag write:
// it answers "do I already hold these exact bytes" before a transfer, where the
// essence hash stays the dedup oracle across retags. Like the essence lookup,
// a single-file CUE album returns one item per virtual track sharing the file.
// Returns an empty slice, not an error, on a clean miss.
func (s *Store) ItemsByContentHash(ctx context.Context, hash string) ([]*model.ItemView, error) {
	const op = "store.ItemsByContentHash"
	if hash == "" {
		return nil, nil
	}
	rows, err := s.read.QueryContext(ctx, itemSelect+` WHERE pi.id IN (
		SELECT itf.item_id FROM item_file itf
		JOIN file f2 ON f2.id = itf.file_id
		WHERE f2.content_hash = ?)`, hash)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return collectItems(rows, op)
}

// ItemsByRecordingMBID returns every track item whose recording MBID matches
// (case-insensitively), an empty slice on a miss. Several are a tie for the caller to
// settle: a recording legitimately appears on both a single and a compilation, and
// resolving the id to an arbitrary one of them would rebuild a shared playlist against
// the wrong local item.
func (s *Store) ItemsByRecordingMBID(ctx context.Context, mbid string) ([]*model.ItemView, error) {
	const op = "store.ItemsByRecordingMBID"
	if mbid == "" {
		return nil, nil
	}
	rows, err := s.read.QueryContext(ctx,
		itemSelect+" WHERE pi.kind = 'track' AND t.mbid = ? COLLATE NOCASE ORDER BY pi.pid", mbid)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return collectItems(rows, op)
}

// ItemsByArtistKey returns the track items whose artist entity has the given match key,
// the seed set for the track descriptive rung. The artist entity is joinable and its
// match_key is portable (identity.MatchKey of the name); the caller filters the seed by
// title/duration in Go. album.match_key embeds a local folder path, so albums are
// NOT joinable and are compared as denormalized text by the caller instead.
func (s *Store) ItemsByArtistKey(ctx context.Context, artistMatchKey string) ([]*model.ItemView, error) {
	const op = "store.ItemsByArtistKey"
	if artistMatchKey == "" {
		return nil, nil
	}
	rows, err := s.read.QueryContext(ctx,
		itemSelect+" WHERE pi.kind = 'track' AND t.artist_id = (SELECT id FROM artist WHERE match_key = ?)",
		artistMatchKey)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return collectItems(rows, op)
}

// ItemsByBookIdent returns every book item matching any of the supplied strong ids
// (release MBID, ASIN, ISBN), each compared case-insensitively and only when non-empty,
// with ItemsByRecordingMBID's rule for several. All three match reliably cross-catalog:
// MBID and ASIN vary only by case, and the ISBN is compared on book.isbn_key, the
// canonical form stored beside the raw value, so a hyphenated and an unhyphenated
// spelling of one book do compare equal.
func (s *Store) ItemsByBookIdent(ctx context.Context, mbid, asin, isbn string) ([]*model.ItemView, error) {
	const op = "store.ItemsByBookIdent"
	if mbid == "" && asin == "" && isbn == "" {
		return nil, nil
	}
	rows, err := s.read.QueryContext(ctx,
		itemSelect+` WHERE pi.kind = 'book' AND (
			 (? <> '' AND bk.mbid = ? COLLATE NOCASE)
		  OR (? <> '' AND bk.asin = ? COLLATE NOCASE)
		  OR (? <> '' AND bk.isbn_key = ?)) ORDER BY pi.pid`,
		mbid, mbid, asin, asin, identity.ISBNKey(isbn), identity.ISBNKey(isbn))
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return collectItems(rows, op)
}

// ItemsByAuthorKey returns the book items whose author entity has the given match key,
// the seed set for the book descriptive rung. Book authors are artist entities keyed by
// identity.MatchKey(name), mirroring ItemsByArtistKey for tracks.
func (s *Store) ItemsByAuthorKey(ctx context.Context, authorMatchKey string) ([]*model.ItemView, error) {
	const op = "store.ItemsByAuthorKey"
	if authorMatchKey == "" {
		return nil, nil
	}
	rows, err := s.read.QueryContext(ctx,
		itemSelect+" WHERE pi.kind = 'book' AND bk.author_id = (SELECT id FROM artist WHERE match_key = ?)",
		authorMatchKey)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return collectItems(rows, op)
}

// identitySelect builds a portable identity descriptor for each item: essence, the
// kind-appropriate strong ids, the view-COALESCE'd artist/title/album, the effective
// duration, and the whole-file fingerprint. The duration mirrors itemViewCols exactly
// (a book's total across its parts, else the item's effective/window duration, else the
// feed-declared episode duration), so an exported ref's duration agrees with the
// ItemView it is later matched against. The fingerprint join is gated on
// pf.start_frames IS NULL, so a virtual/CUE track exports NO fingerprint: one shared
// whole-file fingerprint backs N virtual tracks, and exporting it would let the
// fingerprint rung resolve any of them to an arbitrary sibling. Virtual tracks fall to
// the descriptive rung instead, which uses each track's own artist/title. fingerprint
// is 1:1 on the file PK, so the join never fans a row out.
const identitySelect = `SELECT pi.pid, pi.kind, f.essence_hash,
	COALESCE(NULLIF(t.mbid,''), bk.mbid, ''),
	COALESCE(bk.asin,''), COALESCE(bk.isbn,''),
	COALESCE(NULLIF(t.artist,''), bk.author, pod.title, ''),
	pi.title,
	COALESCE(NULLIF(t.album,''), srs.name, pod.title, ''),
	COALESCE(bk.total_duration_ms, ` + itemEffectiveDurationExpr + `, ep.duration_ms), fp.fp, fp.algo_version,
	fp.duration_bucket` +
	itemJoins + ` LEFT JOIN fingerprint fp ON fp.file_id = f.id AND pf.start_frames IS NULL`

// ItemIdentities returns a portable identity descriptor for each pid that names an
// item, keyed by pid: the export half of the playlist round-trip, which maps a
// playlist's entries through it so an item listed twice exports twice.
func (s *Store) ItemIdentities(ctx context.Context, pids []model.PID) (map[model.PID]model.PortableRef, error) {
	const op = "store.ItemIdentities"
	if len(pids) == 0 {
		return nil, nil
	}
	unique := uniquePIDs(pids)
	byPID := make(map[model.PID]model.PortableRef, len(unique))
	err := chunkSlice(unique, idBatchSize, func(chunk []model.PID) error {
		args := make([]any, len(chunk))
		for i, pid := range chunk {
			args[i] = string(pid)
		}
		rows, err := s.read.QueryContext(ctx, identitySelect+" WHERE pi.pid IN "+placeholders(len(chunk)), args...)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		defer rows.Close()
		for rows.Next() {
			var pid, kind string
			var essence sql.NullString
			var mbid, asin, isbn, artist, title, album string
			var dur, algo, bucket sql.NullInt64
			var fp []byte
			if err := rows.Scan(&pid, &kind, &essence, &mbid, &asin, &isbn,
				&artist, &title, &album, &dur, &fp, &algo, &bucket); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			ref := model.PortableRef{
				Kind:       model.Kind(kind),
				Essence:    essence.String,
				MBID:       mbid,
				ASIN:       asin,
				ISBN:       isbn,
				Artist:     artist,
				Title:      title,
				Album:      album,
				DurationMS: dur.Int64,
			}
			if len(fp) > 0 {
				ref.Fingerprint = fp
				ref.FingerprintAlgo = int(algo.Int64)
				ref.FingerprintBucket = bucket.Int64
			}
			byPID[model.PID(pid)] = ref
		}
		return waxerr.Wrap(waxerr.CodeIO, op, rows.Err())
	})
	if err != nil {
		return nil, err
	}
	return byPID, nil
}

// FingerprintCandidatesByProbe finds catalog files that share at least minShared
// min-hash terms with an EXTERNAL query fingerprint (from another catalog's ref) and
// fall in the given duration-bucket window under the given algorithm. It is the probe
// variant of FingerprintCandidates: the query file's self-join is replaced with literal
// terms, so there is no in-catalog query file to exclude. The caller must supply the
// terms via fingerprint.TermsForAlgo (the same dispatch the write side uses) and pass
// the matching algorithm, so a pure-Go probe never scores against a Chromaprint vector.
// When kind is non-empty only items of that kind are returned, so a book ref never
// resolves to a track item or vice versa (a track is not an alt encoding of a book).
// It returns nil for an empty term set, because IN () is a SQLite syntax error and a
// zero-term (short/corrupt) fingerprint has nothing to match anyway.
//
// The item join is gated on pf.start_frames IS NULL, mirroring the export side: a
// single-file CUE album's shared file is backed by N virtual-track primary edges, so an
// ungated join would fan every term row out N-fold (inflating the shared count) and
// collapse to an arbitrary sibling under GROUP BY. Gating to whole-file edges yields one
// (or zero) row per file, primary or alternate (a whole-file edge belongs to one item),
// so the count is exact and a virtual-track-backed file resolves to an empty item pid
// (skipped by the caller), never an arbitrary sibling.
func (s *Store) FingerprintCandidatesByProbe(ctx context.Context, kind model.Kind, algo, bucketLo, bucketHi int, terms []int64, minShared int) ([]model.FingerprintCandidate, error) {
	const op = "store.FingerprintCandidatesByProbe"
	if len(terms) == 0 {
		return nil, nil
	}
	if minShared < 1 {
		minShared = 1
	}
	args := make([]any, 0, len(terms)+4)
	for _, t := range terms {
		args = append(args, t)
	}
	args = append(args, algo, bucketLo, bucketHi)
	kindFilter := ""
	if kind != "" {
		kindFilter = " AND pi.kind = ?"
		args = append(args, string(kind))
	}
	args = append(args, minShared)
	stmt := `
SELECT f.pid, COALESCE(pi.pid, ''), cf.fp, cf.algo_version, COUNT(*) AS shared
FROM fingerprint_term ct
JOIN fingerprint cf        ON cf.file_id = ct.file_id
JOIN file f                ON f.id = ct.file_id
LEFT JOIN item_file pf     ON pf.file_id = f.id AND pf.role IN ('primary', 'alternate') AND pf.start_frames IS NULL
LEFT JOIN playable_item pi ON pi.id = pf.item_id
WHERE ct.term IN ` + placeholders(len(terms)) + `
  AND cf.algo_version = ?
  AND cf.duration_bucket BETWEEN ? AND ?` + kindFilter + `
GROUP BY ct.file_id
HAVING shared >= ?
ORDER BY shared DESC`
	rows, err := s.read.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.FingerprintCandidate
	for rows.Next() {
		var c model.FingerprintCandidate
		var fpid, ipid string
		if err := rows.Scan(&fpid, &ipid, &c.FP, &c.AlgoVersion, &c.SharedTerms); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		c.FilePID, c.ItemPID = model.PID(fpid), model.PID(ipid)
		out = append(out, c)
	}
	return out, rows.Err()
}

// collectItems drains rows into a slice of item views.
func collectItems(rows *sql.Rows, op string) ([]*model.ItemView, error) {
	defer rows.Close()
	var out []*model.ItemView
	for rows.Next() {
		v, err := scanItemView(rows)
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return out, nil
}
