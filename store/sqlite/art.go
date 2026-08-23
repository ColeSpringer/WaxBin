package sqlite

import (
	"container/list"
	"context"
	"database/sql"
	"errors"
	"sync"

	"github.com/colespringer/waxbin/art"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// thumbCacheMax and thumbCacheBytes bound the in-process thumbnail cache. The entry
// count alone is not enough: a resolve above a source's own size re-encodes at that
// size, so an entry is only "a few KB" at the small rungs, and a library of large
// covers browsed at a hero rung would otherwise pin hundreds of full-size images.
const (
	thumbCacheMax   = 256
	thumbCacheBytes = 64 << 20
)

// thumbFailMax bounds the negative cache. It is separate from the thumbnail cache and
// deliberately small: failures are cheap to hold and cheap to rebuild, and a library of
// covers no decoder here reads (embedded AVIF/HEIC, which carry declared dimensions and
// so reach the generator) must not flush the real thumbnails generated beside them.
const (
	thumbFailMax = 64
	// Negative entries carry no bytes, so the count is the only bound that applies.
	thumbFailBytes = 0
)

// thumbCache is a bounded in-process LRU of generated thumbnails keyed by (source
// hash, size). It sits in front of the thumb_cache table: it serves a read-only store,
// which cannot persist to the table and would otherwise regenerate on every request,
// and it saves a read-write store a SQL round-trip and re-decode for a hot cover. It
// is safe for concurrent use.
type thumbCache struct {
	mu       sync.Mutex
	max      int
	maxBytes int
	bytes    int
	ll       *list.List // front = most recently used; values are *thumbEntry
	items    map[thumbKey]*list.Element
}

type thumbKey struct {
	hash string
	size int
}

type thumbEntry struct {
	key   thumbKey
	blob  model.ArtBlob // Bytes is treated as immutable once cached
	bytes int           // len(blob.Bytes), the entry's charge against the byte bound
}

func newThumbCache(max, maxBytes int) *thumbCache {
	return &thumbCache{max: max, maxBytes: maxBytes, ll: list.New(), items: map[thumbKey]*list.Element{}}
}

// get returns a cached thumbnail blob and marks it most-recently-used. A nil cache
// (defensive) is a permanent miss. The returned blob's Bytes is a private copy: the
// cache shares no backing array with callers, so a caller that mutates the bytes
// cannot corrupt the cached entry (or another caller's view of it).
func (c *thumbCache) get(hash string, size int) (model.ArtBlob, bool) {
	if c == nil {
		return model.ArtBlob{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[thumbKey{hash, size}]; ok {
		c.ll.MoveToFront(el)
		return cloneArtBlob(el.Value.(*thumbEntry).blob), true
	}
	return model.ArtBlob{}, false
}

// cloneArtBlob returns a copy of b whose Bytes shares no backing array with the
// original, so the cache and its callers cannot mutate each other's bytes.
func cloneArtBlob(b model.ArtBlob) model.ArtBlob {
	if b.Bytes != nil {
		b.Bytes = append([]byte(nil), b.Bytes...)
	}
	return b
}

// put inserts or refreshes a thumbnail, evicting the least-recently-used entries
// past the bound. It stores a private copy of the blob's bytes so a later mutation of
// the caller's slice cannot reach the cache.
func (c *thumbCache) put(hash string, size int, blob model.ArtBlob) {
	if c == nil {
		return
	}
	stored := cloneArtBlob(blob)
	c.mu.Lock()
	defer c.mu.Unlock()
	key := thumbKey{hash, size}
	if el, ok := c.items[key]; ok {
		e := el.Value.(*thumbEntry)
		c.bytes += len(stored.Bytes) - e.bytes
		e.blob, e.bytes = stored, len(stored.Bytes)
		c.ll.MoveToFront(el)
		c.evict()
		return
	}
	c.items[key] = c.ll.PushFront(&thumbEntry{key: key, blob: stored, bytes: len(stored.Bytes)})
	c.bytes += len(stored.Bytes)
	c.evict()
}

// evict drops least-recently-used entries until the cache is inside both bounds. It
// always keeps the most recent entry, so a single image larger than the byte bound is
// still served rather than evicting itself on the way in.
func (c *thumbCache) evict() {
	for c.ll.Len() > 1 && (c.ll.Len() > c.max || c.bytes > c.maxBytes) {
		oldest := c.ll.Back()
		if oldest == nil {
			return
		}
		c.ll.Remove(oldest)
		e := oldest.Value.(*thumbEntry)
		c.bytes -= e.bytes
		delete(c.items, e.key)
	}
}

// attachArtTxChanged maps a track/book item's cover onto the 'track' art slot
// (keyed by the item id) and reports whether the mapping changed, for the
// music/audiobook write paths that emit a delta only on a real change. See
// attachEntityArtTxChanged for the shared body.
func attachArtTxChanged(ctx context.Context, tx *sql.Tx, itemID int64, img *model.ArtImage) (bool, error) {
	return attachEntityArtTxChanged(ctx, tx, "track", itemID, img)
}

// storableArt fills whatever an ingest carrier left empty from the bytes it already
// holds: the content address, the format, and the dimensions, each independently of the
// others. Independently, because zero dimensions read as undecodable to ResolveArt,
// which then serves the source unscaled forever, so a format with no dimensions is a gap
// to fill rather than an impossible state. It returns a shallow copy when it fills
// anything, so a producer's own value is never mutated behind its back.
func storableArt(img *model.ArtImage) *model.ArtImage {
	if img == nil || len(img.Data) == 0 {
		return img
	}
	if img.Hash != "" && img.Format != "" && img.Width != 0 && img.Height != 0 {
		return img
	}
	info := art.Describe(img.Data)
	cp := *img
	if cp.Hash == "" {
		cp.Hash = info.Hash
	}
	if cp.Format == "" {
		cp.Format = info.Format
	}
	if cp.Width == 0 {
		cp.Width = info.Width
	}
	if cp.Height == 0 {
		cp.Height = info.Height
	}
	return &cp
}

// insertArtSourceTx dedups a decoded/probed cover into the content-addressed
// art_source store (keyed by content hash), a no-op when the source is already
// present. It is the single art-blob writer shared by the front-cover attach and the
// role-scoped entity-art set.
func insertArtSourceTx(ctx context.Context, tx *sql.Tx, img *model.ArtImage) error {
	_, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO art_source(hash, format, width, height, size, data, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		img.Hash, img.Format, img.Width, img.Height, len(img.Data), img.Data, nowNS())
	return err
}

// attachEntityArtTx is the error-only wrapper over attachEntityArtTxChanged for the
// callers that do not need the changed signal.
func attachEntityArtTx(ctx context.Context, tx *sql.Tx, entityType string, entityID int64, img *model.ArtImage) error {
	_, err := attachEntityArtTxChanged(ctx, tx, entityType, entityID, img)
	return err
}

// attachEntityArtTxChanged dedups a front-cover image into the content-addressed art
// store and maps it to one entity (entity_type, entity_id). It backs every cover
// ingest: a track/book item ('track'), a podcast feed ('podcast'), an episode
// ('episode'), and, from enrichment, a release group and a matched album's own
// pressing.
//
// The album slot needs care: album art is otherwise derived on read from the current
// track maps, which keeps a re-cover or delete from leaving a stale cover behind, and a
// stored album row wins over that derivation (artInChain consults it first). Only a
// writer that knows WHICH edition the album is should use it, today the release match.
//
// The write is idempotent: an entity already mapping this cover does nothing (reporting
// false), so it can run on every scan/sync without churn. A nil or empty image is a
// no-op, which is the "no cover found" signal every automatic producer uses; an image
// carrying bytes is always stored, with anything it left undescribed filled from those
// bytes. It touches the front role alone, so a re-sync cannot clobber a user's
// back/booklet.
func attachEntityArtTxChanged(ctx context.Context, tx *sql.Tx, entityType string, entityID int64, img *model.ArtImage) (bool, error) {
	if img == nil || len(img.Data) == 0 {
		return false, nil
	}
	// Before the hash comparison below, so an undescribed cover matching the stored one
	// re-attributes in place rather than being read as a different picture. The
	// attribution is checked here too: the same-hash branch below writes it without
	// going through setEntityArtRoleTx, so this is the guard for that path.
	img = storableArt(img)
	if err := checkArtImage(img, entityType, string(model.ArtRoleFront)); err != nil {
		return false, err
	}
	var curHash sql.NullString
	if err := tx.QueryRowContext(ctx,
		"SELECT source_hash FROM art_map WHERE entity_type = ? AND entity_id = ? AND role = 'front'",
		entityType, entityID).Scan(&curHash); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if curHash.Valid && curHash.String == img.Hash {
		// The same picture, possibly from a new origin: a feed that rotated its image
		// URL, or a cover that was a sidecar and is now embedded in the tags. Refresh
		// the attribution in place rather than leaving a dead URL on the row, and still
		// report no change, since the image the entity shows did not move. The UPDATE
		// is conditional, so a genuine no-op rescan writes nothing at all.
		return false, refreshArtProvenanceTx(ctx, tx, entityType, entityID, string(model.ArtRoleFront), img)
	}
	// Re-point this entity's front cover through the shared slot writer; an entity
	// has exactly one image per role. When the old cover loses its last referencing
	// map row it becomes an orphaned source for GCArt.
	if err := setEntityArtRoleTx(ctx, tx, entityType, entityID, string(model.ArtRoleFront), img); err != nil {
		return false, err
	}
	return true, nil
}

// attachEntityArtUnlessLockedTx is attachEntityArtTx guarded by the entity's "art"
// curation lock: the automatic entity-cover writers (a release-group enrichment, a
// podcast feed sync) call it so a cover the user chose survives a forced re-run or an
// image-URL change in the feed. A nil image is a no-op, and costs no lock lookup.
//
// It is the last guard, not the first. A caller that would pay to produce the image
// should check the lock before doing so: the podcast sync reads it off the show
// (model.Podcast.CoverLocked) and skips the download entirely, rather than fetching
// megabytes for this to discard on every sync while the lock stands.
//
// The lock, not the provenance, is what governs the write. Provenance stays purely
// descriptive, so a future producer that legitimately stamps "user" cannot quietly
// change who is allowed to overwrite what.
func attachEntityArtUnlessLockedTx(ctx context.Context, tx *sql.Tx, entityType string, entityID int64, img *model.ArtImage) error {
	if img == nil || len(img.Data) == 0 {
		return nil
	}
	locked, err := entityFieldLockedTx(ctx, tx, entityType, entityID, "art")
	if err != nil {
		return err
	}
	if locked {
		return nil
	}
	return attachEntityArtTx(ctx, tx, entityType, entityID, img)
}

// refreshArtProvenanceTx re-attributes an existing mapping whose image is unchanged.
// It writes only when some column actually differs, so it cannot churn updated_at on a
// rescan that read the same cover from the same place.
func refreshArtProvenanceTx(ctx context.Context, tx *sql.Tx, entityType string, entityID int64, role string, img *model.ArtImage) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE art_map SET source = ?, provider = ?, source_url = ?, updated_at = ?
		 WHERE entity_type = ? AND entity_id = ? AND role = ?
		   AND (source <> ? OR provider <> ? OR source_url <> ?)`,
		string(img.Source), img.Provider, img.SourceURL, nowNS(),
		entityType, entityID, role,
		string(img.Source), img.Provider, img.SourceURL)
	return err
}

// fillAlbumArtTx attaches an album's front cover only when the album resolves none at
// all, which is the rule SetEntityArt's doc states for enrichment ("fills entity covers
// only when empty"). Fill-when-empty is the first of two guards between a provider and
// a cover the user already has; the entity's "art" lock is the second, and it is what
// covers the case fill-when-empty cannot see: a deliberately cleared cover, which
// resolves nothing and would otherwise read as an invitation to fill.
//
// "Empty" has to mean what ResolveArt means. An album normally owns no art_map row and
// answers from a member track's embedded cover instead (artInChain's derived rung), and a
// stored album row wins over that derivation. Probing only for the album's own row would
// therefore call almost every album empty and quietly replace the file's own artwork with
// the Cover Art Archive's on the first release match.
//
// It is separate from attachEntityArtTxChanged, which re-points on any differing hash,
// because that behaviour belongs to the writers whose source is the file itself: a scan's
// embedded cover should follow a retag.
func fillAlbumArtTx(ctx context.Context, tx *sql.Tx, albumID int64, img *model.ArtImage) error {
	var resolves int
	if err := tx.QueryRowContext(ctx,
		`SELECT CASE WHEN `+albumResolvesFrontArt+` THEN 1 ELSE 0 END FROM album al WHERE al.id = ?`,
		albumID).Scan(&resolves); err != nil {
		return err
	}
	if resolves == 1 {
		return nil
	}
	return attachEntityArtUnlessLockedTx(ctx, tx, string(model.ArtAlbum), albumID, img)
}

// clearAlbumArtTx drops an album's own front-cover row, returning it to whatever the
// derived rung answers. It undoes a release match's cover; a member track's embedded
// cover is untouched, since nothing here wrote it.
func clearAlbumArtTx(ctx context.Context, tx *sql.Tx, albumID int64) error {
	_, err := tx.ExecContext(ctx,
		"DELETE FROM art_map WHERE entity_type = ? AND entity_id = ? AND role = 'front'",
		string(model.ArtAlbum), albumID)
	return err
}

// artLevel is one rung of the resolution fallback chain: an entity type and its
// internal id.
type artLevel struct {
	typ string
	id  int64
}

// ResolveArt resolves art for an entity in one role. The front role walks the
// fallback chain from the requested level up toward the root (track -> album ->
// release_group -> artist -> genre) and answers from the first level that has a
// front cover; every other role resolves at the requested level alone, since an
// ancestor's back cover or booklet would be misleading for a descendant. An empty
// role means front. size selects the output: a non-positive size returns the stored
// source exactly, and a positive size returns an image the caller can draw, scaled to
// fit a square box with that maximum side when the source is larger and re-encoded at
// its own size when the source already fits but is held in a format outside
// art.Displayable. Generated images are cached and never upscaled. A source that
// cannot be decoded at all is returned as stored, whatever its format. The blob
// carries the answering Level and, for an album answered from a member track's
// cover, Derived. CodeNotFound means no consulted level has art in that role.
func (s *Store) ResolveArt(ctx context.Context, ref model.EntityRef, role model.ArtRole, size int) (*model.ArtBlob, error) {
	const op = "store.ResolveArt"
	// The blob is built from the metadata-only read's own answer rather than from a
	// second walk of the chain, so the two reads cannot drift apart about which level
	// answered or where its picture came from.
	prov, err := s.artProvenance(ctx, ref, role, op)
	if err != nil {
		return nil, err
	}
	source := func() (*model.ArtBlob, error) {
		data, err := s.artSourceData(ctx, prov.SourceHash, op)
		if err != nil {
			return nil, err
		}
		return &model.ArtBlob{
			Bytes: data, Format: prov.Format, Width: prov.Width, Height: prov.Height,
			SourceHash: prov.SourceHash, Level: prov.Level, Derived: prov.Derived,
			Attribution: prov.Attribution, UpdatedAt: prov.UpdatedAt,
		}, nil
	}

	// Original requested: serve the source. Nothing above this point has loaded it,
	// which is what keeps a grid of cached thumbnails from reading a full-size image
	// out of blob storage per cover and discarding it.
	if size <= 0 {
		return source()
	}
	// Dimensions unknown (an undecodable or exotic source, e.g. an AVIF/HEIC cover with
	// no pure-Go decoder): there is nothing to decode, so serve the original and let the
	// caller decide what to do with a format it may not paint.
	longest := max(prov.Width, prov.Height)
	if longest == 0 {
		return source()
	}
	// A source already inside the box needs no scaling, but a sized request is a request
	// to draw the picture, so a format a client cannot paint is re-encoded at its own
	// size rather than served as stored.
	if longest <= size && art.Displayable(prov.Format) {
		return source()
	}
	// The box is passed through as asked rather than clamped to the dimensions above.
	// Every rung at or above the source's own size yields the same bytes, so clamping
	// would collapse them onto one cache entry, but those dimensions are only as good as
	// whatever wrote them: storableArt keeps a producer's own values and fills only what
	// it left zero, so a cover whose stored size understates its bytes would be scaled
	// down to the stored figure and quietly answer a large request with a small picture.
	// art.Thumbnail decodes the real bytes and never upscales, so passing the box
	// through is right whatever the row says. The cost is that the cache keys on
	// whatever sizes clients ask for rather than one entry per cover, so what bounds
	// thumb_cache is `db thumbs`, not the size of the library.
	thumb, err := s.thumbnail(ctx, prov, size, source)
	if err != nil {
		return nil, err
	}
	// The thumbnail cache is keyed by (source, size) alone; the same cached bytes can
	// answer different levels (a track's own cover vs a sibling resolving it through the
	// album), and two entities sharing one deduped source can report different
	// provenance, so both are stamped per request rather than cached. thumbCache.put
	// stores the blob above this line, which is what makes this the single stamp site.
	// Format and the dimensions are overwritten by the generator, which is exactly what
	// separates a thumbnail from the stored source ArtProvenance describes.
	thumb.Level, thumb.Derived = prov.Level, prov.Derived
	thumb.Attribution, thumb.UpdatedAt = prov.Attribution, prov.UpdatedAt
	return thumb, nil
}

// ArtProvenance answers where an entity's art in one role came from, without loading
// the picture. It resolves exactly as ResolveArt does, chain fallback and all, and
// reports the level that answered, whether an album's answer was derived from a member
// track, and the stored source's address, format, dimensions, byte size and
// attribution. The blob's overflow pages are never touched, which is the whole saving:
// a detail screen that only draws a provenance mark stops paying for the image.
//
// It carries no lock. A lock belongs to the entity that was asked about rather than to
// whichever chain level answered, so a caller that wants one reads ArtLocked. Like
// ResolveArt and ArtRoles it has no proxy surface: a read never needs the write lock the
// socket exists to share. CodeNotFound means no consulted level has art in that role,
// the same answer ResolveArt gives.
func (s *Store) ArtProvenance(ctx context.Context, ref model.EntityRef, role model.ArtRole) (*model.ArtProvenance, error) {
	return s.artProvenance(ctx, ref, role, "store.ArtProvenance")
}

// artProvenance is the shared body, taking its caller's op so a ResolveArt failure
// still reports itself as one.
func (s *Store) artProvenance(ctx context.Context, ref model.EntityRef, role model.ArtRole, op string) (*model.ArtProvenance, error) {
	hit, role, err := s.artHitFor(ctx, ref, role, op)
	if err != nil {
		return nil, err
	}
	format, w, h, size, err := s.artSourceMeta(ctx, hit.hash, op)
	if err != nil {
		return nil, err
	}
	return &model.ArtProvenance{
		Role: role, Level: model.ArtEntity(hit.level), Derived: hit.derived, SourceHash: hit.hash,
		Format: format, Width: w, Height: h, Size: size,
		Attribution: model.Attribution{Source: hit.source, Provider: hit.provider, SourceURL: hit.sourceURL},
		UpdatedAt:   hit.updatedAt,
	}, nil
}

// artHitFor validates a reference and role and walks the fallback chain for them,
// returning the answering hit and the normalized role. It is the one chain walk behind
// both art reads.
func (s *Store) artHitFor(ctx context.Context, ref model.EntityRef, role model.ArtRole, op string) (artHit, model.ArtRole, error) {
	if !ref.Type.Valid() {
		return artHit{}, role, waxerr.New(waxerr.CodeInvalid, op, "unknown art entity type: "+string(ref.Type))
	}
	if role == "" {
		role = model.ArtRoleFront
	}
	if !role.Valid() {
		return artHit{}, role, waxerr.New(waxerr.CodeInvalid, op, "unknown art role: "+string(role))
	}
	chain, err := s.artChain(ctx, ref)
	if err != nil {
		return artHit{}, role, err
	}
	// Non-front roles never inherit from an ancestor: truncate the chain to the
	// requested entity itself (always the first level artChain builds).
	if role != model.ArtRoleFront && len(chain) > 1 {
		chain = chain[:1]
	}
	hit, ok, err := s.artInChain(ctx, chain, role)
	if err != nil {
		return artHit{}, role, err
	}
	if !ok {
		return artHit{}, role, waxerr.New(waxerr.CodeNotFound, op,
			"no "+string(role)+" art for "+string(ref.Type)+":"+string(ref.PID))
	}
	return hit, role, nil
}

// ArtRoles lists the artwork slots an entity holds at its own level, with no
// chain fallback: each stored role with its source's format, dimensions, and
// hash, plus where that attachment came from, in role order.
//
// A Locked entry with an empty SourceHash is a lock with no artifact behind it, so a
// renderer must check SourceHash before trying to fetch bytes. That is what a cleared
// and locked cover looks like, the state that stops a feed or an enrichment pass
// refilling the slot, and reporting it is the whole point: it is otherwise invisible
// and every later `art set` on the entity is refused. The lock is the base fact and
// the artifact is the overlay, the same inversion FieldProvenance makes on the item
// side. So an entity with no art returns an empty list only when it is also unlocked.
//
// A nonexistent entity is an error, so a caller can still tell that apart from
// "nothing stored". `waxbin art roles` is the CLI face of it. There is no proxy
// surface, since a read never needs the write lock the socket exists to share.
func (s *Store) ArtRoles(ctx context.Context, ref model.EntityRef) ([]model.ArtRoleInfo, error) {
	const op = "store.ArtRoles"
	if !ref.Type.Valid() {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "unknown art entity type: "+string(ref.Type))
	}
	// artChain resolves the pid to its internal id (erroring on an unknown entity);
	// the first level is always the requested entity itself.
	chain, err := s.artChain(ctx, ref)
	if err != nil {
		return nil, err
	}
	rows, err := s.read.QueryContext(ctx,
		`SELECT m.role, s.format, s.width, s.height, s.hash,
		        m.source, m.provider, m.source_url, m.updated_at
		 FROM art_map m JOIN art_source s ON s.hash = m.source_hash
		 WHERE m.entity_type = ? AND m.entity_id = ? ORDER BY m.role`,
		chain[0].typ, chain[0].id)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.ArtRoleInfo
	for rows.Next() {
		var info model.ArtRoleInfo
		var role, source string
		if err := rows.Scan(&role, &info.Format, &info.Width, &info.Height, &info.SourceHash,
			&source, &info.Provider, &info.SourceURL, &info.UpdatedAt); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		info.Role, info.Source = model.ArtRole(role), model.ProvenanceSource(source)
		out = append(out, info)
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	// The "art" lock guards the front cover alone, matching the item-side art lock, so
	// only that row reports it. One extra read, taken unconditionally because a lock
	// with no front row is exactly the state worth reporting. It reads whichever table
	// governs this entity type, so a track cover locked by SetItemArt reports here too
	// (artLockIsItemScoped).
	lock, err := artLockTx(ctx, s.read, ref.Type, chain[0].id)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if !lock.Locked {
		return out, nil
	}
	for i := range out {
		if out[i].Role == model.ArtRoleFront {
			out[i].Locked = true
			return out, nil
		}
	}
	// Locked with nothing attached. Synthesize the front entry from the lock row alone,
	// carrying its recorded source and timestamp rather than zero values, the way
	// FieldProvenance does for a lock-only row. The rows come back ordered by role name,
	// where front sorts after back/background/booklet/disc, so it goes last.
	return append(out, model.ArtRoleInfo{
		Role:        model.ArtRoleFront,
		Attribution: lock.Attribution,
		UpdatedAt:   lock.UpdatedAt, Locked: true,
	}), nil
}

// thumbnail returns a cached thumbnail for (source hash, size), generating and caching
// it on a miss. source loads the original image, and is called only when generation is
// actually needed: a cache hit at either level answers without touching the blob at all.
// A generation failure (e.g. an exotic source format) falls back to the original and is
// remembered in a separate small cache, so the decode is not attempted again for those
// bytes at that box.
func (s *Store) thumbnail(ctx context.Context, prov *model.ArtProvenance, size int,
	source func() (*model.ArtBlob, error)) (*model.ArtBlob, error) {
	const op = "store.ResolveArt"
	hash := prov.SourceHash
	// Generation already failed for these bytes at this box. The blob load still happens,
	// since the original is what gets served either way; what this skips is the repeated
	// decode attempt and the repeated warning behind it.
	if _, failed := s.thumbFail.get(hash, size); failed {
		return source()
	}
	// Check the in-process cache next. This serves a read-only store, which cannot
	// persist to thumb_cache and would otherwise regenerate on every request, and it
	// saves a read-write store the SQL round-trip and re-decode for a hot cover.
	if blob, ok := s.thumbMem.get(hash, size); ok {
		b := blob
		return &b, nil
	}

	var data []byte
	var format string
	var w, h int
	err := s.read.QueryRowContext(ctx,
		"SELECT data, format, width, height FROM thumb_cache WHERE source_hash = ? AND size = ?",
		hash, size).Scan(&data, &format, &w, &h)
	if err == nil {
		blob := model.ArtBlob{Bytes: data, Format: format, Width: w, Height: h, SourceHash: hash, Thumbnail: true}
		s.thumbMem.put(hash, size, blob)
		b := blob
		return &b, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	src, err := source()
	if err != nil {
		return nil, err
	}
	thumb, tFormat, tw, th, gerr := art.Thumbnail(src.Bytes, size)
	if gerr != nil {
		// The pure-Go decoders cannot handle this source (a truncated image, or an
		// exotic format such as AVIF/HEIC that arrived with dimensions declared): serve
		// the original unscaled rather than failing. Remembering the failure does not
		// save the blob load, since the original is what gets served, but it does stop a
		// grid scroll re-attempting the decode and re-emitting this warning per request.
		// It goes in its own cache so a run of such covers cannot flush the thumbnails
		// generated beside them.
		s.log.Warn("art thumbnail generation failed; serving original", "hash", hash, "size", size, "err", gerr)
		s.thumbFail.put(hash, size, model.ArtBlob{})
		return src, nil
	}

	blob := model.ArtBlob{Bytes: thumb, Format: tFormat, Width: tw, Height: th, SourceHash: hash, Thumbnail: true}
	s.thumbMem.put(hash, size, blob)

	// Best-effort cache write: a read-only library still serves the generated thumbnail
	// from the in-process cache above, just without persisting it to disk.
	if !s.readOnly {
		if err := s.writeTx(ctx, func(tx *sql.Tx) error {
			_, e := tx.ExecContext(ctx,
				`INSERT OR REPLACE INTO thumb_cache(source_hash, size, format, width, height, data, created_at)
				 VALUES (?,?,?,?,?,?,?)`, hash, size, tFormat, tw, th, thumb, nowNS())
			return e
		}); err != nil {
			s.log.Warn("caching thumbnail", "hash", hash, "size", size, "err", err)
		}
	}
	b := blob
	return &b, nil
}

// artSourceMeta loads a source image's format, dimensions and byte length by hash,
// deliberately not its data: the blob's overflow pages stay untouched, which is what
// makes the metadata-only read cheap.
func (s *Store) artSourceMeta(ctx context.Context, hash, op string) (format string, w, h, size int, err error) {
	err = s.read.QueryRowContext(ctx,
		"SELECT format, width, height, size FROM art_source WHERE hash = ?", hash).Scan(&format, &w, &h, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, 0, 0, waxerr.New(waxerr.CodeNotFound, op, "art source missing: "+hash)
	}
	if err != nil {
		return "", 0, 0, 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return format, w, h, size, nil
}

// artSourceData loads a source image's bytes by hash.
func (s *Store) artSourceData(ctx context.Context, hash, op string) ([]byte, error) {
	var data []byte
	err := s.read.QueryRowContext(ctx,
		"SELECT data FROM art_source WHERE hash = ?", hash).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, op, "art source missing: "+hash)
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return data, nil
}

// artHit is what one art-chain walk found: the source hash to serve, the level that
// answered, whether that answer was derived from a member track, and the answering
// art_map row's provenance.
type artHit struct {
	hash      string
	level     string
	derived   bool
	source    model.ProvenanceSource
	provider  string
	sourceURL string
	updatedAt int64
}

// artInChain returns the first chain level that has art in the requested role;
// ok=false when none does. The role filter is what keeps a level's back/booklet slots
// from answering a front-cover walk (any-role rows used to win by rowid order). The
// primary key holds one row per (entity, role), so a direct lookup needs no
// ordering. Album front art with no direct entry is derived from the album's
// track covers, so it is always a cover currently carried by a track in that
// album, never a stale denormalized mapping left by a re-cover, cross-album
// retag, or deletion; the member ORDER BY rowid keeps the pick stable. The
// derivation applies to the front cover alone: the other roles answer from a
// durable row at the entity's own level or not at all, matching the public
// contract that non-front roles never look past the requested entity.
//
// Both branches read the provenance columns, so a derived album cover honestly
// reports the member track's own answer rather than claiming the album chose it.
func (s *Store) artInChain(ctx context.Context, chain []artLevel, role model.ArtRole) (artHit, bool, error) {
	const op = "store.ResolveArt"
	for _, lv := range chain {
		if lv.id == 0 {
			continue
		}
		// A fresh hit per rung: Scan leaves its destinations untouched on ErrNoRows, so
		// reusing one across rungs would let a partly-filled miss answer for the rung
		// that follows it.
		var h artHit
		var src string
		qerr := s.read.QueryRowContext(ctx,
			"SELECT source_hash, source, provider, source_url, updated_at FROM art_map"+
				" WHERE entity_type = ? AND entity_id = ? AND role = ?",
			lv.typ, lv.id, string(role)).Scan(&h.hash, &src, &h.provider, &h.sourceURL, &h.updatedAt)
		if qerr == nil {
			h.level, h.source = lv.typ, model.ProvenanceSource(src)
			return h, true, nil
		}
		if !errors.Is(qerr, sql.ErrNoRows) {
			return artHit{}, false, waxerr.Wrap(waxerr.CodeIO, op, qerr)
		}
		if lv.typ == string(model.ArtAlbum) && role == model.ArtRoleFront {
			var d artHit
			derr := s.read.QueryRowContext(ctx,
				`SELECT tm.source_hash, tm.source, tm.provider, tm.source_url, tm.updated_at
				 FROM art_map tm JOIN track t ON t.item_id = tm.entity_id
				 WHERE tm.entity_type = 'track' AND tm.role = 'front' AND t.album_id = ?
				 ORDER BY tm.rowid LIMIT 1`, lv.id).Scan(&d.hash, &src, &d.provider, &d.sourceURL, &d.updatedAt)
			if derr == nil {
				d.level, d.source, d.derived = lv.typ, model.ProvenanceSource(src), true
				return d, true, nil
			}
			if !errors.Is(derr, sql.ErrNoRows) {
				return artHit{}, false, waxerr.Wrap(waxerr.CodeIO, op, derr)
			}
		}
	}
	return artHit{}, false, nil
}

// artChain builds the resolution fallback chain for a reference: the requested
// level first, then its ancestors up to the genre root, each as (type, internal
// id). Missing ancestors are dropped, so the chain only contains resolvable levels.
// A returned chain is never empty: every branch resolves the requested entity
// itself before appending ancestors, and an unknown pid or type errors instead.
// ResolveArt's non-front truncation (chain[:1]) and ArtRoles' own-level read
// (chain[0]) both rely on that.
func (s *Store) artChain(ctx context.Context, ref model.EntityRef) ([]artLevel, error) {
	const op = "store.ResolveArt"
	switch ref.Type {
	case model.ArtTrack:
		return s.trackArtChain(ctx, ref.PID)
	case model.ArtAlbum:
		albumID, err := s.idByPID(ctx, "album", ref.PID, op)
		if err != nil {
			return nil, err
		}
		return s.albumArtChain(ctx, albumID)
	case model.ArtReleaseGroup:
		rgID, err := s.idByPID(ctx, "release_group", ref.PID, op)
		if err != nil {
			return nil, err
		}
		return s.releaseGroupArtChain(ctx, rgID)
	case model.ArtArtist:
		artistID, err := s.idByPID(ctx, "artist", ref.PID, op)
		if err != nil {
			return nil, err
		}
		return []artLevel{{string(model.ArtArtist), artistID}}, nil
	case model.ArtGenre:
		genreID, err := s.idByPID(ctx, "genre", ref.PID, op)
		if err != nil {
			return nil, err
		}
		return []artLevel{{string(model.ArtGenre), genreID}}, nil
	case model.ArtEpisode:
		return s.episodeArtChain(ctx, ref.PID)
	case model.ArtPodcast:
		podID, err := s.idByPID(ctx, "podcast", ref.PID, op)
		if err != nil {
			return nil, err
		}
		return []artLevel{{string(model.ArtPodcast), podID}}, nil
	case model.ArtPlaylist:
		plID, err := s.idByPID(ctx, "playlist", ref.PID, op)
		if err != nil {
			return nil, err
		}
		// One rung on purpose: a playlist has no ancestor, so even a front cover has
		// nowhere to fall back to.
		return []artLevel{{string(model.ArtPlaylist), plID}}, nil
	}
	return nil, waxerr.New(waxerr.CodeInvalid, op, "unknown art entity type: "+string(ref.Type))
}

// episodeArtChain resolves a podcast episode's chain: episode -> podcast. The
// episode's own artwork wins; a feed image is the fallback for an episode without
// one.
func (s *Store) episodeArtChain(ctx context.Context, pid model.PID) ([]artLevel, error) {
	const op = "store.ResolveArt"
	var itemID, podcastID int64
	err := s.read.QueryRowContext(ctx,
		`SELECT pi.id, ep.podcast_id FROM playable_item pi
		 JOIN episode ep ON ep.item_id = pi.id WHERE pi.pid = ?`, string(pid)).Scan(&itemID, &podcastID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, op, "no such episode: "+string(pid))
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return []artLevel{
		{string(model.ArtEpisode), itemID},
		{string(model.ArtPodcast), podcastID},
	}, nil
}

// trackArtChain resolves a track's full chain: track -> album -> release_group ->
// artist -> genre. The artist level is the release group's primary artist, falling
// back to the track's album artist then artist; the genre level is the item's first
// genre.
func (s *Store) trackArtChain(ctx context.Context, pid model.PID) ([]artLevel, error) {
	const op = "store.ResolveArt"
	var itemID int64
	var kind string
	var albumID, albumArtistID, artistID sql.NullInt64
	err := s.read.QueryRowContext(ctx,
		`SELECT pi.id, pi.kind, t.album_id, t.album_artist_id, t.artist_id
		 FROM playable_item pi LEFT JOIN track t ON t.item_id = pi.id WHERE pi.pid = ?`, string(pid)).
		Scan(&itemID, &kind, &albumID, &albumArtistID, &artistID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, op, "no such item: "+string(pid))
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	chain := []artLevel{{string(model.ArtTrack), itemID}}
	// A book stores its cover at the item level (the 'track' art_map slot, keyed by
	// the item id), then falls back to its author artist and first genre. It has no
	// album/release-group rungs, so resolve it directly and return.
	if kind == string(model.KindBook) {
		var authorID sql.NullInt64
		if err := s.read.QueryRowContext(ctx,
			"SELECT author_id FROM book WHERE item_id = ?", itemID).Scan(&authorID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if authorID.Valid {
			chain = append(chain, artLevel{string(model.ArtArtist), authorID.Int64})
		}
		if gid := s.firstItemGenre(ctx, itemID); gid != 0 {
			chain = append(chain, artLevel{string(model.ArtGenre), gid})
		}
		return chain, nil
	}
	var rgID, primaryArtistID int64
	if albumID.Valid {
		chain = append(chain, artLevel{string(model.ArtAlbum), albumID.Int64})
		rgID, primaryArtistID, err = s.albumParents(ctx, albumID.Int64)
		if err != nil {
			return nil, err
		}
		if rgID != 0 {
			chain = append(chain, artLevel{string(model.ArtReleaseGroup), rgID})
		}
	}
	// Artist level: prefer the release group's primary artist, then album artist,
	// then track artist.
	artLevelID := primaryArtistID
	if artLevelID == 0 && albumArtistID.Valid {
		artLevelID = albumArtistID.Int64
	}
	if artLevelID == 0 && artistID.Valid {
		artLevelID = artistID.Int64
	}
	if artLevelID != 0 {
		chain = append(chain, artLevel{string(model.ArtArtist), artLevelID})
	}
	if gid := s.firstItemGenre(ctx, itemID); gid != 0 {
		chain = append(chain, artLevel{string(model.ArtGenre), gid})
	}
	return chain, nil
}

func (s *Store) albumArtChain(ctx context.Context, albumID int64) ([]artLevel, error) {
	chain := []artLevel{{string(model.ArtAlbum), albumID}}
	rgID, artistID, err := s.albumParents(ctx, albumID)
	if err != nil {
		return nil, err
	}
	if rgID != 0 {
		chain = append(chain, artLevel{string(model.ArtReleaseGroup), rgID})
	}
	if artistID != 0 {
		chain = append(chain, artLevel{string(model.ArtArtist), artistID})
	}
	return chain, nil
}

func (s *Store) releaseGroupArtChain(ctx context.Context, rgID int64) ([]artLevel, error) {
	chain := []artLevel{{string(model.ArtReleaseGroup), rgID}}
	var artistID sql.NullInt64
	if err := s.read.QueryRowContext(ctx,
		"SELECT primary_artist_id FROM release_group WHERE id = ?", rgID).Scan(&artistID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.Wrap(waxerr.CodeIO, "store.ResolveArt", err)
	}
	if artistID.Valid {
		chain = append(chain, artLevel{string(model.ArtArtist), artistID.Int64})
	}
	return chain, nil
}

// albumParents returns an album's release-group id and that group's primary-artist
// id (each 0 when absent).
func (s *Store) albumParents(ctx context.Context, albumID int64) (rgID, artistID int64, err error) {
	var rg sql.NullInt64
	if err := s.read.QueryRowContext(ctx,
		"SELECT release_group_id FROM album WHERE id = ?", albumID).Scan(&rg); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, 0, waxerr.Wrap(waxerr.CodeIO, "store.ResolveArt", err)
	}
	if !rg.Valid {
		return 0, 0, nil
	}
	var artist sql.NullInt64
	if err := s.read.QueryRowContext(ctx,
		"SELECT primary_artist_id FROM release_group WHERE id = ?", rg.Int64).Scan(&artist); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return rg.Int64, 0, waxerr.Wrap(waxerr.CodeIO, "store.ResolveArt", err)
	}
	if artist.Valid {
		return rg.Int64, artist.Int64, nil
	}
	return rg.Int64, 0, nil
}

// firstItemGenre returns the item's lowest-id genre, or 0 when it has none.
func (s *Store) firstItemGenre(ctx context.Context, itemID int64) int64 {
	var gid int64
	err := s.read.QueryRowContext(ctx,
		"SELECT genre_id FROM item_genre WHERE item_id = ? ORDER BY genre_id LIMIT 1", itemID).Scan(&gid)
	if err != nil {
		return 0
	}
	return gid
}

// idByPID resolves an entity pid to its rowid in the named table.
func (s *Store) idByPID(ctx context.Context, table string, pid model.PID, op string) (int64, error) {
	var id int64
	err := s.read.QueryRowContext(ctx, "SELECT id FROM "+table+" WHERE pid = ?", string(pid)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, waxerr.New(waxerr.CodeNotFound, op, "no such "+table+": "+string(pid))
	}
	if err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return id, nil
}

// deleteEntityArtTx drops every art_map row an entity holds, in all roles. A delete
// path must call this rather than leave the rows for GCArt: the entity tables use a
// plain INTEGER PRIMARY KEY, and SQLite hands a deleted row's id to the next insert,
// so a surviving map row would show a dead entity's cover on whatever live entity
// inherits its id, and GC would keep the row because the id exists again. It is the
// same reasoning deleteItemCascade applies to entity_enrichment markers. The source
// image left behind is still GC's to reclaim.
func deleteEntityArtTx(ctx context.Context, tx *sql.Tx, entityType string, entityID int64) error {
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM art_map WHERE entity_type = ? AND entity_id = ?", entityType, entityID); err != nil {
		return err
	}
	return deleteEntityArtLockTx(ctx, tx, entityType, entityID)
}

// deleteEntityArtLockTx drops an entity's "art" curation row, for the same reason the
// map rows go: entity_curation is polymorphic with no FK, podcast and playlist rowids
// are reused (INTEGER PRIMARY KEY without AUTOINCREMENT), and deleteOrphanEntity sweeps
// only the four merge entities. A stale lock inherited by a reused id refuses every
// later art set on the new entity and silently skips the feed image, with no surface
// that shows why.
func deleteEntityArtLockTx(ctx context.Context, tx *sql.Tx, entityType string, entityID int64) error {
	_, err := tx.ExecContext(ctx,
		"DELETE FROM entity_curation WHERE entity_type = ? AND entity_id = ? AND field = 'art'",
		entityType, entityID)
	return err
}

// GCArt reclaims orphaned art: map rows whose entity is gone, then source images
// no longer referenced by any map, cascading to their cached thumbnails. It
// returns the number of source images and thumbnails removed. It is the repair for
// the orphan counts VerifyDerived reports.
func (s *Store) GCArt(ctx context.Context) (sources, thumbnails int, err error) {
	const op = "store.GCArt"
	err = s.writeTx(ctx, func(tx *sql.Tx) error {
		// Drop polymorphic map rows whose backing entity no longer exists. This
		// slot->table list and verify.go's liveArtSourceQ arms are the same set and
		// must stay in lockstep: a slot missing on either side leaves a persistent
		// verify complaint that --fix cannot clear (verify counts a source orphaned
		// while GC keeps it, or GC never removes the dead rows keeping it alive).
		// Roles never enter into it; both sides are deliberately role-agnostic.
		for _, m := range []struct{ typ, table string }{
			{"track", "playable_item"}, {"album", "album"},
			{"release_group", "release_group"}, {"artist", "artist"}, {"genre", "genre"},
			{"episode", "playable_item"}, {"podcast", "podcast"},
			{"playlist", "playlist"},
		} {
			if _, e := tx.ExecContext(ctx,
				"DELETE FROM art_map WHERE entity_type = ? AND entity_id NOT IN (SELECT id FROM "+m.table+")",
				m.typ); e != nil {
				return e
			}
		}
		// Count thumbnails about to be cascaded, then drop unreferenced sources.
		if e := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM thumb_cache WHERE source_hash NOT IN (SELECT source_hash FROM art_map)`).
			Scan(&thumbnails); e != nil {
			return e
		}
		r, e := tx.ExecContext(ctx,
			"DELETE FROM art_source WHERE hash NOT IN (SELECT source_hash FROM art_map)")
		if e != nil {
			return e
		}
		n, _ := r.RowsAffected()
		sources = int(n)
		return nil
	})
	if err != nil {
		return 0, 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return sources, thumbnails, nil
}
