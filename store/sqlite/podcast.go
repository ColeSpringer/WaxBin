package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"html"
	"strings"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// EnsurePodcastLibrary finds or creates the internal library for downloaded
// episodes. ModePodcast keeps it out of scan/organize while still letting episode
// files satisfy the file/library foreign key and reuse path/trash code.
func (s *Store) EnsurePodcastLibrary(ctx context.Context, dir string) (int64, error) {
	const op = "store.EnsurePodcastLibrary"
	var id int64
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		root := []byte(dir)
		existing, err := libraryByRootTx(ctx, tx, root)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if existing != nil {
			id = existing.ID
			return nil
		}
		pid := model.NewPID()
		r, err := tx.ExecContext(ctx,
			"INSERT INTO library(pid, root, display_root, mode, profile, created_at) VALUES (?,?,?,?,?,?)",
			string(pid), root, dir, string(model.ModePodcast), "waxbin-native", nowNS())
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		id, _ = r.LastInsertId()
		return appendChange(ctx, tx, "library", pid, model.OpCreate)
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// UpsertFeed persists a parsed feed atomically: the podcast row is created or
// updated by identity key, the feed image is ingested onto it, and every feed
// episode is upserted by its per-podcast key. It does not delete episodes missing
// from a later feed, and it does not move a downloaded episode back to remote.
func (s *Store) UpsertFeed(ctx context.Context, in model.UpsertFeedInput) (*model.UpsertFeedResult, error) {
	const op = "store.UpsertFeed"
	if in.IdentityKey == "" {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "feed has no identity (no guid or url)")
	}
	res := &model.UpsertFeedResult{}
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		now := nowNS()
		podcastID, podcastPID, created, titleChanged, err := upsertPodcast(ctx, tx, in, now)
		if err != nil {
			return err
		}
		res.PodcastPID, res.Created = podcastPID, created

		// Ingest the feed image onto the podcast entity (idempotent on hash).
		if err := attachEntityArtTx(ctx, tx, "podcast", podcastID, in.Image); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}

		// A retitled show changes every episode's FTS subtitle (artist/album), so force
		// a per-episode rewrite that round; otherwise an unchanged episode is skipped
		// entirely to avoid write churn on a large feed re-sync.
		podKey := in.IdentityKey
		for i := range in.Feed.Episodes {
			fe := in.Feed.Episodes[i]
			key := identity.EpisodeKey(podKey, fe.GUID, fe.EnclosureURL, fe.Title)
			if key == "" {
				continue // nothing to key on: skip rather than collapse untitled items
			}
			added, changed, err := upsertEpisode(ctx, tx, podcastID, in.Feed.Title, key, fe, false, false, now, titleChanged)
			if err != nil {
				return err
			}
			switch {
			case added:
				res.EpisodesAdded++
			case changed:
				res.EpisodesUpdated++
			}
		}

		op := model.OpUpdate
		if created {
			op = model.OpCreate
		}
		return appendChange(ctx, tx, "podcast", podcastPID, op)
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// UpsertShow creates or updates a show without enumerating a feed: a manual show, or
// a provider-backed channel added before its first enumeration. Episodes are added
// separately via UpsertEpisode. It returns the show pid and whether it was created.
func (s *Store) UpsertShow(ctx context.Context, in model.UpsertShowInput) (model.PID, bool, error) {
	const op = "store.UpsertShow"
	if in.IdentityKey == "" {
		return "", false, waxerr.New(waxerr.CodeInvalid, op, "show has no identity key")
	}
	if in.SourceType != "" && !in.SourceType.ValidShowSource() {
		return "", false, waxerr.New(waxerr.CodeInvalid, op, "invalid show source type: "+string(in.SourceType))
	}
	var pid model.PID
	var created bool
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		now := nowNS()
		fi := model.UpsertFeedInput{
			FeedURL: in.FeedURL, IdentityKey: in.IdentityKey, SourceType: in.SourceType,
			Feed: model.Feed{
				Title: in.Title, Author: in.Author, Description: in.Description,
				Link: in.Link, ImageURL: in.ImageURL,
			},
		}
		id, ppid, cr, _, err := upsertPodcast(ctx, tx, fi, now)
		if err != nil {
			return err
		}
		pid, created = ppid, cr
		if err := attachEntityArtTx(ctx, tx, "podcast", id, in.Image); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return appendChange(ctx, tx, "podcast", ppid, opFor(cr))
	})
	return pid, created, err
}

// UpsertEpisode adds or updates a single episode under an existing show, bypassing a
// feed sync. Pinned keeps the episode out of retention pruning. The episode is keyed
// within its show with the same rule used for feed episodes, so a later feed sync of
// the same show updates it rather than forking it.
func (s *Store) UpsertEpisode(ctx context.Context, in model.UpsertEpisodeInput) (*model.UpsertEpisodeResult, error) {
	const op = "store.UpsertEpisode"
	res := &model.UpsertEpisodeResult{}
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		var podcastID int64
		var podKey, podTitle string
		err := tx.QueryRowContext(ctx,
			"SELECT id, identity_key, title FROM podcast WHERE pid = ?", string(in.PodcastPID)).
			Scan(&podcastID, &podKey, &podTitle)
		if errors.Is(err, sql.ErrNoRows) {
			return waxerr.New(waxerr.CodeNotFound, op, "no such podcast: "+string(in.PodcastPID))
		}
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		key := identity.EpisodeKey(podKey, in.Episode.GUID, in.Episode.EnclosureURL, in.Episode.Title)
		if key == "" {
			return waxerr.New(waxerr.CodeInvalid, op, "episode has no usable identity (guid, enclosure, or title)")
		}
		now := nowNS()
		added, changed, err := upsertEpisode(ctx, tx, podcastID, podTitle, key, in.Episode, in.Pinned, true, now, false)
		if err != nil {
			return err
		}
		res.Created = added
		// Resolve the (possibly pre-existing) episode pid to return it.
		if err := tx.QueryRowContext(ctx,
			"SELECT pid FROM playable_item WHERE kind='episode' AND identity_key=?", key).
			Scan(&res.EpisodePID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		// Emit the show delta only when the episode actually changed, so a no-op re-upsert
		// stays change_log-silent (the episode's own item delta is emitted by upsertEpisode).
		if added || changed {
			return appendChange(ctx, tx, "podcast", in.PodcastPID, model.OpUpdate)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// upsertPodcast inserts or updates a podcast row by identity_key, preserving its
// pid/created_at and (when the feed omits a value) prior metadata. Returns the row
// id, pid, whether it was newly created, and whether its title changed (which
// forces an episode-FTS refresh, since the title is each episode's FTS subtitle).
func upsertPodcast(ctx context.Context, tx *sql.Tx, in model.UpsertFeedInput, now int64) (int64, model.PID, bool, bool, error) {
	const op = "store.UpsertFeed"
	f := in.Feed
	st := in.SourceType
	if st == "" {
		st = model.SourceRSS // older or unspecified feeds are rss subscriptions
	}
	var id int64
	var pid, oldTitle, oldKey string
	err := tx.QueryRowContext(ctx,
		"SELECT id, pid, title, identity_key FROM podcast WHERE identity_key = ?", in.IdentityKey).Scan(&id, &pid, &oldTitle, &oldKey)
	if errors.Is(err, sql.ErrNoRows) {
		// The identity key can flip when a feed that was subscribed without a
		// <podcast:guid> later publishes one (feed:URL -> pguid:...). Fall back to the
		// feed URL so a re-add/OPML-reimport updates the existing row (and adopts the new
		// key) instead of hitting UNIQUE(feed_url) on a blind INSERT.
		err = tx.QueryRowContext(ctx,
			"SELECT id, pid, title, identity_key FROM podcast WHERE feed_url = ?", in.FeedURL).Scan(&id, &pid, &oldTitle, &oldKey)
	}
	switch {
	case err == nil:
		// When the show's identity key changes (the feed_url-matched flip above), re-key
		// its existing episodes to the new show prefix in the same tx. Otherwise the feed
		// loop that follows keys every episode under the NEW ep: prefix, finds no match,
		// and re-inserts the whole catalog as duplicates (orphaning the downloaded rows).
		if oldKey != in.IdentityKey {
			if err := rekeyEpisodes(ctx, tx, id, oldKey, in.IdentityKey); err != nil {
				return 0, "", false, false, waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE podcast SET
			identity_key=?, feed_url=?, title=?, sort_key=?, author=?, description=?, link=?, language=?,
			category=?, explicit=?, image_url=?, guid=?, etag=?, last_modified=?, source_type=?,
			last_fetched_at=?, updated_at=? WHERE id=?`,
			in.IdentityKey, in.FeedURL, f.Title, model.SortKey(f.Title), f.Author, f.Description, f.Link, f.Language,
			f.Category, boolInt(f.Explicit), f.ImageURL, f.GUID, in.ETag, in.LastModified,
			string(st), in.FetchedAtNS, now, id); err != nil {
			return 0, "", false, false, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return id, model.PID(pid), false, oldTitle != f.Title, nil
	case !errors.Is(err, sql.ErrNoRows):
		return 0, "", false, false, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	newPID := model.NewPID()
	r, err := tx.ExecContext(ctx, `INSERT INTO podcast
		(pid, feed_url, identity_key, title, sort_key, author, description, link, language,
		 category, explicit, image_url, guid, etag, last_modified, source_type,
		 last_fetched_at, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		string(newPID), in.FeedURL, in.IdentityKey, f.Title, model.SortKey(f.Title), f.Author,
		f.Description, f.Link, f.Language, f.Category, boolInt(f.Explicit), f.ImageURL, f.GUID,
		in.ETag, in.LastModified, string(st), in.FetchedAtNS, now, now)
	if err != nil {
		return 0, "", false, false, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	id, _ = r.LastInsertId()
	return id, newPID, true, false, nil
}

// upsertEpisode inserts or updates one episode item and subtype. It reads the
// stored row first so identical feed items do not rewrite the database or FTS, and
// it never moves a downloaded episode back to remote.
func upsertEpisode(ctx context.Context, tx *sql.Tx, podcastID int64, podcastTitle, key string, fe model.FeedEpisode, pinned, managePinned bool, now int64, forceWrite bool) (added, changed bool, err error) {
	const op = "store.UpsertFeed"
	var itemID int64
	var itemPID string
	var stored storedEpisode
	var explicitInt, pinnedInt int64
	// One query fetches existence, pid, and every stored field used for the change
	// check. Unchanged episodes cost one point lookup; changed ones need no extra
	// pid select before emitting a delta.
	scanErr := tx.QueryRowContext(ctx, `SELECT pi.id, pi.pid, pi.title,
		COALESCE(e.guid,''), COALESCE(e.description,''), COALESCE(e.link,''),
		COALESCE(e.pub_date,0), COALESCE(e.year,0), COALESCE(e.season,0), COALESCE(e.episode_no,0),
		COALESCE(e.episode_type,''), COALESCE(e.duration_ms,0), COALESCE(e.explicit,0),
		COALESCE(e.enclosure_url,''), COALESCE(e.enclosure_type,''), COALESCE(e.enclosure_size,0),
		COALESCE(e.transcript_url,''), COALESCE(e.transcript_type,''), COALESCE(e.chapters_url,''),
		COALESCE(e.image_url,''), COALESCE(e.pinned,0)
		FROM playable_item pi LEFT JOIN episode e ON e.item_id = pi.id
		WHERE pi.kind='episode' AND pi.identity_key=?`, key).Scan(&itemID, &itemPID, &stored.title,
		&stored.guid, &stored.description, &stored.link, &stored.pubDate, &stored.year, &stored.season,
		&stored.episodeNo, &stored.episodeType, &stored.durationMS, &explicitInt,
		&stored.enclosureURL, &stored.enclosureType, &stored.enclosureSize,
		&stored.transcriptURL, &stored.transcriptType, &stored.chaptersURL, &stored.imageURL, &pinnedInt)
	exists := scanErr == nil
	if scanErr != nil && !errors.Is(scanErr, sql.ErrNoRows) {
		return false, false, waxerr.Wrap(waxerr.CodeIO, op, scanErr)
	}
	stored.explicit = explicitInt != 0
	stored.pinned = pinnedInt != 0

	if exists {
		// Preserve user-edited episode fields against a feed re-sync: overlay the stored
		// value for every locked field onto the incoming item BEFORE the change check, so
		// a locked-field feed change is neither written nor treated as a reason to rewrite.
		if locked, err := lockedFieldSetTx(ctx, tx, itemID); err != nil {
			return false, false, waxerr.Wrap(waxerr.CodeIO, op, err)
		} else if len(locked) > 0 {
			fe = overlayLockedEpisodeFields(fe, stored, locked)
		}
		// Unchanged: write nothing and emit no delta (state, which may be present for a
		// downloaded episode, is untouched throughout). pinned participates in the change
		// check only when the caller manages it (managePinned): a feed re-sync passes
		// pinned=false but does NOT manage pinning, so it never un-pins nor churns a
		// user-pinned episode; the explicit UpsertEpisode path DOES manage it, so it can
		// pin or un-pin an existing episode.
		if !forceWrite && stored.equals(fe) && (!managePinned || stored.pinned == pinned) {
			return false, false, nil
		}
		// The update never writes playable_item.state, so a downloaded (present) episode
		// is not knocked back to remote by a metadata re-sync.
		if _, err := tx.ExecContext(ctx,
			"UPDATE playable_item SET title=?, sort_key=?, updated_at=? WHERE id=?",
			fe.Title, model.SortKey(fe.Title), now, itemID); err != nil {
			return false, false, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := writeEpisodeRow(ctx, tx, itemID, podcastID, fe, pinned, managePinned, now); err != nil {
			return false, false, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := syncEpisodeSearchFTS(ctx, tx, itemID, fe, podcastTitle); err != nil {
			return false, false, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := appendChange(ctx, tx, "item", model.PID(itemPID), model.OpUpdate); err != nil {
			return false, false, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return false, true, nil
	}

	// New episode: insert the item (state=remote) and its subtype + FTS.
	pid := model.NewPID()
	r, err := tx.ExecContext(ctx, `INSERT INTO playable_item
		(pid, kind, state, title, sort_key, identity_key, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		string(pid), string(model.KindEpisode), string(model.StateRemote),
		fe.Title, model.SortKey(fe.Title), key, now, now)
	if err != nil {
		return false, false, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	itemID, _ = r.LastInsertId()
	if err := writeEpisodeRow(ctx, tx, itemID, podcastID, fe, pinned, managePinned, now); err != nil {
		return false, false, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if err := syncEpisodeSearchFTS(ctx, tx, itemID, fe, podcastTitle); err != nil {
		return false, false, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if err := appendChange(ctx, tx, "item", pid, model.OpCreate); err != nil {
		return false, false, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return true, true, nil
}

// editEpisodeFieldsTx applies user edits to a podcast episode. It writes the episode
// row columns and the title on playable_item, then rebuilds the episode's search row.
// Provenance, auto-lock, and the item delta are recorded by the shared edit caller.
func editEpisodeFieldsTx(ctx context.Context, tx *sql.Tx, itemID int64, fields []string, edits map[string]string, op string) error {
	var touchTitle bool
	var newTitle string
	setCols := make([]string, 0, len(fields))
	args := make([]any, 0, len(fields)+2)
	for _, f := range fields {
		switch f {
		case "title":
			touchTitle, newTitle = true, edits[f]
		case "description":
			setCols, args = append(setCols, "description=?"), append(args, edits[f])
		case "link":
			setCols, args = append(setCols, "link=?"), append(args, edits[f])
		case "season":
			n, err := parseIntField(edits[f], "season", op)
			if err != nil {
				return err
			}
			setCols, args = append(setCols, "season=?"), append(args, nullInt(n))
		case "episode_no":
			n, err := parseIntField(edits[f], "episode_no", op)
			if err != nil {
				return err
			}
			setCols, args = append(setCols, "episode_no=?"), append(args, nullInt(n))
		case "episode_type":
			et := edits[f]
			if et == "" {
				et = string(model.EpisodeFull)
			}
			if !validEpisodeType(et) {
				return waxerr.New(waxerr.CodeInvalid, op, "episode_type must be full|trailer|bonus: "+et)
			}
			setCols, args = append(setCols, "episode_type=?"), append(args, et)
		case "explicit":
			b, err := parseBoolField(edits[f], "explicit", op)
			if err != nil {
				return err
			}
			setCols, args = append(setCols, "explicit=?"), append(args, boolInt(b))
		case "pinned":
			b, err := parseBoolField(edits[f], "pinned", op)
			if err != nil {
				return err
			}
			setCols, args = append(setCols, "pinned=?"), append(args, boolInt(b))
		default:
			return waxerr.New(waxerr.CodeInvalid, op, "unhandled episode field: "+f)
		}
	}

	now := nowNS()
	if touchTitle {
		if _, err := tx.ExecContext(ctx,
			"UPDATE playable_item SET title=?, sort_key=?, updated_at=? WHERE id=?",
			newTitle, model.SortKey(newTitle), now, itemID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	}
	if len(setCols) > 0 {
		setCols = append(setCols, "updated_at=?")
		args = append(args, now, itemID)
		if _, err := tx.ExecContext(ctx,
			"UPDATE episode SET "+strings.Join(setCols, ", ")+" WHERE item_id=?", args...); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	}
	return rebuildEpisodeSearchFTSTx(ctx, tx, itemID)
}

// validEpisodeType reports whether t is one of the three feed episode types.
func validEpisodeType(t string) bool {
	switch model.EpisodeType(t) {
	case model.EpisodeFull, model.EpisodeTrailer, model.EpisodeBonus:
		return true
	default:
		return false
	}
}

// rebuildEpisodeSearchFTSTx reloads an episode's current title/description and its
// show title, then rewrites its search row (so an edited title/description is
// searchable).
func rebuildEpisodeSearchFTSTx(ctx context.Context, tx *sql.Tx, itemID int64) error {
	var title, description, podcastTitle string
	err := tx.QueryRowContext(ctx, `SELECT pi.title, ep.description, pod.title
		FROM playable_item pi JOIN episode ep ON ep.item_id = pi.id
		JOIN podcast pod ON pod.id = ep.podcast_id WHERE pi.id = ?`, itemID).
		Scan(&title, &description, &podcastTitle)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, "store.EditItemFields", err)
	}
	return syncEpisodeSearchFTS(ctx, tx, itemID, model.FeedEpisode{Title: title, Description: description}, podcastTitle)
}

// overlayLockedEpisodeFields returns fe with every user-locked field replaced by the
// stored value, so a feed re-sync neither overwrites a curated episode edit nor
// counts a locked-field feed change as a reason to rewrite the row.
func overlayLockedEpisodeFields(fe model.FeedEpisode, stored storedEpisode, locked map[string]bool) model.FeedEpisode {
	if locked["title"] {
		fe.Title = stored.title
	}
	if locked["description"] {
		fe.Description = stored.description
	}
	if locked["link"] {
		fe.Link = stored.link
	}
	if locked["season"] {
		fe.Season = int(stored.season)
	}
	if locked["episode_no"] {
		fe.EpisodeNo = int(stored.episodeNo)
	}
	if locked["episode_type"] {
		fe.EpisodeType = model.EpisodeType(stored.episodeType)
	}
	if locked["explicit"] {
		fe.Explicit = stored.explicit
	}
	return fe
}

// rekeyEpisodes rewrites the per-show prefix of every episode identity_key when a
// show's own identity_key changes (e.g. feed:URL -> pguid:...), so the episodes stay
// matched to their show rather than re-inserting as duplicates on the next upsert. An
// episode key is "ep:<showKey>\x1f<id>"; only the <showKey> prefix changes. SQLite
// length()/substr() operate on characters, so the rewrite is correct for a multibyte
// key. The <id> parts are unchanged, so the re-keyed rows stay unique.
func rekeyEpisodes(ctx context.Context, tx *sql.Tx, podcastID int64, oldKey, newKey string) error {
	if oldKey == "" || oldKey == newKey {
		return nil
	}
	oldPrefix := "ep:" + oldKey + "\x1f"
	newPrefix := "ep:" + newKey + "\x1f"
	_, err := tx.ExecContext(ctx, `UPDATE playable_item
		SET identity_key = ? || substr(identity_key, length(?) + 1)
		WHERE kind = 'episode'
		  AND id IN (SELECT item_id FROM episode WHERE podcast_id = ?)
		  AND substr(identity_key, 1, length(?)) = ?`,
		newPrefix, oldPrefix, podcastID, oldPrefix, oldPrefix)
	return err
}

// storedEpisode holds the persisted episode fields used to detect whether an
// incoming feed item actually changed, so an unchanged re-sync writes nothing.
type storedEpisode struct {
	title, guid, description, link, episodeType          string
	pubDate, year, season, episodeNo, durationMS         int64
	enclosureSize                                        int64
	explicit, pinned                                     bool
	enclosureURL, enclosureType                          string
	transcriptURL, transcriptType, chaptersURL, imageURL string
}

// equals reports whether the stored episode matches an incoming feed item across
// every persisted field (so a difference in any of them triggers a rewrite).
func (e storedEpisode) equals(fe model.FeedEpisode) bool {
	return e.title == fe.Title &&
		e.guid == fe.GUID &&
		e.description == fe.Description &&
		e.link == fe.Link &&
		e.pubDate == fe.PubDateNS &&
		e.year == int64(fe.Year) &&
		e.season == int64(fe.Season) &&
		e.episodeNo == int64(fe.EpisodeNo) &&
		e.episodeType == string(episodeTypeOr(fe.EpisodeType)) &&
		e.durationMS == fe.DurationMS &&
		e.explicit == fe.Explicit &&
		e.enclosureURL == fe.EnclosureURL &&
		e.enclosureType == fe.EnclosureType &&
		e.enclosureSize == fe.EnclosureSize &&
		e.transcriptURL == fe.TranscriptURL &&
		e.transcriptType == fe.TranscriptType &&
		e.chaptersURL == fe.ChaptersURL &&
		e.imageURL == fe.ImageURL
}

// writeEpisodeRow upserts the episode subtype row (shared by the create and update
// paths). It never touches playable_item.state, so a downloaded episode stays present
// across a metadata re-sync. On the conflict update, pinned is rewritten only when the
// caller manages it (managePinned): a feed re-sync passes managePinned=false and so
// never un-pins a user-pinned episode, while the explicit UpsertEpisode path passes
// true and can pin/un-pin. A new episode always inserts with the given pinned value.
func writeEpisodeRow(ctx context.Context, tx *sql.Tx, itemID, podcastID int64, fe model.FeedEpisode, pinned, managePinned bool, now int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO episode
		(item_id, podcast_id, guid, description, link, pub_date, year, season, episode_no,
		 episode_type, duration_ms, explicit, enclosure_url, enclosure_type, enclosure_size,
		 transcript_url, transcript_type, chapters_url, image_url, pinned, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(item_id) DO UPDATE SET
			podcast_id=excluded.podcast_id, guid=excluded.guid, description=excluded.description,
			link=excluded.link, pub_date=excluded.pub_date, year=excluded.year, season=excluded.season,
			episode_no=excluded.episode_no, episode_type=excluded.episode_type,
			duration_ms=excluded.duration_ms, explicit=excluded.explicit,
			enclosure_url=excluded.enclosure_url, enclosure_type=excluded.enclosure_type,
			enclosure_size=excluded.enclosure_size, transcript_url=excluded.transcript_url,
			transcript_type=excluded.transcript_type, chapters_url=excluded.chapters_url,
			image_url=excluded.image_url,
			pinned=CASE WHEN ? THEN excluded.pinned ELSE pinned END,
			updated_at=excluded.updated_at`,
		itemID, podcastID, fe.GUID, fe.Description, fe.Link, nullInt64(fe.PubDateNS), nullInt(fe.Year),
		nullInt(fe.Season), nullInt(fe.EpisodeNo), string(episodeTypeOr(fe.EpisodeType)),
		nullInt64(fe.DurationMS), boolInt(fe.Explicit), fe.EnclosureURL, fe.EnclosureType,
		fe.EnclosureSize, fe.TranscriptURL, fe.TranscriptType, fe.ChaptersURL, fe.ImageURL,
		boolInt(pinned), now, now, boolInt(managePinned))
	return err
}

// syncEpisodeSearchFTS rebuilds an episode's metadata FTS row: the title carries
// the heavy weight, the podcast title stands in as artist/album, and the
// description goes to the low-weight extra field. The description is HTML-stripped
// first (feeds often put markup in content:encoded), so search matches the prose,
// not tag names like "href" or "span". The transcript lives in transcript_fts (a
// separate table) so a title hit outranks a body hit.
func syncEpisodeSearchFTS(ctx context.Context, tx *sql.Tx, itemID int64, fe model.FeedEpisode, podcastTitle string) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM search_fts WHERE rowid = ?", itemID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx,
		"INSERT INTO search_fts(rowid, kind, title, subtitle, artist, album, extra) VALUES (?,?,?,?,?,?,?)",
		itemID, string(model.KindEpisode), fe.Title, "", podcastTitle, podcastTitle, stripHTML(fe.Description))
	return err
}

// stripHTML removes HTML tags and decodes entities, reducing a marked-up feed
// description to the searchable prose underneath. It is a no-op for plain text.
func stripHTML(s string) string {
	if !strings.ContainsAny(s, "<&") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
			b.WriteByte(' ') // a tag boundary separates words: "a<br>b" -> "a b"
		case !inTag:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(html.UnescapeString(b.String())), " ")
}

// AttachEpisodeFile records a downloaded enclosure: it creates the file row in the
// podcast library, makes it the episode's primary file, flips the episode to
// present, and ingests any episode artwork. A prior download for the same episode
// is replaced (its file row removed) so a re-download does not leak a row.
// PutEpisodeChapters stores an episode's Podcasting-2.0 chapters (source
// 'podcast_url') against its downloaded file, replacing only the URL-sourced rows so
// a re-sync cannot clobber richer chapters with poorer embedded ones. It emits an
// item delta on a real change. The episode must be downloaded (have a primary file).
func (s *Store) PutEpisodeChapters(ctx context.Context, episodePID model.PID, chapters []model.Chapter) error {
	const op = "store.PutEpisodeChapters"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		var itemID, fileID int64
		err := tx.QueryRowContext(ctx, `SELECT pi.id, f.id FROM playable_item pi
			JOIN item_file pf ON pf.item_id = pi.id AND pf.role = 'primary'
			JOIN file f ON f.id = pf.file_id
			WHERE pi.pid = ? AND pi.kind = 'episode'`, string(episodePID)).Scan(&itemID, &fileID)
		if errors.Is(err, sql.ErrNoRows) {
			return waxerr.New(waxerr.CodeNotFound, op, "episode not downloaded: "+string(episodePID))
		}
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		changed, err := syncChaptersForFileSource(ctx, tx, itemID, fileID, "podcast_url", chapters)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if changed {
			return appendChange(ctx, tx, "item", episodePID, model.OpUpdate)
		}
		return nil
	})
}

func (s *Store) AttachEpisodeFile(ctx context.Context, in model.AttachEpisodeFileInput) (model.PID, error) {
	const op = "store.AttachEpisodeFile"
	var filePID model.PID
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		itemID, kind, err := s.itemIDKindByPIDTx(ctx, tx, in.EpisodePID, op)
		if err != nil {
			return err
		}
		if kind != string(model.KindEpisode) {
			return waxerr.New(waxerr.CodeInvalid, op, "item is not an episode: "+string(in.EpisodePID))
		}

		// Replace any prior download: drop the existing primary file row (its
		// item_file edge cascades) so the new one is the sole backing file.
		if err := deletePrimaryFileTx(ctx, tx, itemID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}

		now := nowNS()
		pid := model.NewPID()
		fileID, err := insertFileRow(ctx, tx, in.LibraryID, pid, in.File, now)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		filePID = pid
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO item_file(item_id, file_id, role, position) VALUES (?, ?, 'primary', 0)",
			itemID, fileID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE playable_item SET state=?, updated_at=? WHERE id=?",
			string(model.StatePresent), now, itemID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := attachEntityArtTx(ctx, tx, "episode", itemID, in.Image); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := appendChange(ctx, tx, "file", filePID, model.OpCreate); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return appendChange(ctx, tx, "item", in.EpisodePID, model.OpUpdate)
	})
	if err != nil {
		return "", err
	}
	return filePID, nil
}

// DropEpisodeFile removes an episode's downloaded file from the catalog and returns
// it to remote, the catalog half of retention. The play_state row is keyed on the
// item (which survives), so resume/finished state is preserved across a download/
// retention cycle. The on-disk file is removed by the caller (the prune path
// bypasses the trash to reclaim space). It is a no-op for an episode with no file.
func (s *Store) DropEpisodeFile(ctx context.Context, episodePID model.PID) error {
	const op = "store.DropEpisodeFile"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		itemID, kind, err := s.itemIDKindByPIDTx(ctx, tx, episodePID, op)
		if err != nil {
			return err
		}
		if kind != string(model.KindEpisode) {
			return waxerr.New(waxerr.CodeInvalid, op, "item is not an episode: "+string(episodePID))
		}
		var filePID model.PID
		err = tx.QueryRowContext(ctx,
			`SELECT f.pid FROM item_file itf JOIN file f ON f.id = itf.file_id
			 WHERE itf.item_id = ? AND itf.role = 'primary'`, itemID).Scan(&filePID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // not downloaded: nothing to drop
		}
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := deletePrimaryFileTx(ctx, tx, itemID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE playable_item SET state=?, updated_at=? WHERE id=?",
			string(model.StateRemote), nowNS(), itemID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := appendChange(ctx, tx, "file", filePID, model.OpDelete); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return appendChange(ctx, tx, "item", episodePID, model.OpUpdate)
	})
}

// PutTranscript stores an episode's transcript body and indexes it in
// transcript_fts, keyed by the episode item id.
func (s *Store) PutTranscript(ctx context.Context, in model.PutTranscriptInput) error {
	const op = "store.PutTranscript"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		itemID, kind, err := s.itemIDKindByPIDTx(ctx, tx, in.EpisodePID, op)
		if err != nil {
			return err
		}
		if kind != string(model.KindEpisode) {
			return waxerr.New(waxerr.CodeInvalid, op, "item is not an episode: "+string(in.EpisodePID))
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO episode_transcript(item_id, format, body, source_url, created_at)
			 VALUES (?,?,?,?,?)
			 ON CONFLICT(item_id) DO UPDATE SET format=excluded.format, body=excluded.body,
				source_url=excluded.source_url`,
			itemID, in.Format, in.Body, in.SourceURL, nowNS()); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		// Rebuild the transcript FTS row (episode_id stored, body indexed).
		if _, err := tx.ExecContext(ctx, "DELETE FROM transcript_fts WHERE episode_id = ?", itemID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO transcript_fts(episode_id, body) VALUES (?, ?)", itemID, in.Body); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return appendChange(ctx, tx, "item", in.EpisodePID, model.OpUpdate)
	})
}

// RemovePodcast unsubscribes: it deletes the podcast, all its episode items (and
// via cascade their files/edges/play_state/transcript), and orphaned art. It
// returns the display paths of any downloaded files the caller should remove from
// disk to reclaim space. The logical history is intentionally not kept here, since
// unsubscribing is an explicit user action distinct from retention.
func (s *Store) RemovePodcast(ctx context.Context, podcastPID model.PID) ([]string, error) {
	const op = "store.RemovePodcast"
	var files []string
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		podcastID, err := idByPIDTx(ctx, tx, "podcast", podcastPID, op)
		if err != nil {
			return err
		}
		// All the episode items of this podcast, reused as a subquery by the bulk
		// deletes below so a feed with thousands of episodes drops in a handful of
		// statements rather than O(episodes) round-trips.
		const epItems = "SELECT item_id FROM episode WHERE podcast_id = ?"

		// 1. Collect downloaded file paths before deleting, so the caller can reclaim
		//    them from disk.
		rows, err := tx.QueryContext(ctx,
			"SELECT f.display_path FROM item_file itf JOIN file f ON f.id = itf.file_id WHERE itf.item_id IN ("+epItems+")",
			podcastID)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if p != "" {
				files = append(files, p)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		rows.Close()

		// 2. Emit one 'item' delete delta per episode (read pi.pid before the rows are
		//    gone) so delta-sync consumers drop each episode from their caches.
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO change_log(ts, entity_type, entity_pid, op) "+
				"SELECT ?, 'item', pi.pid, 'delete' FROM playable_item pi WHERE pi.id IN ("+epItems+")",
			nowNS(), podcastID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}

		// 3-6. Bulk-delete the derived/backing state, then the items (which cascades the
		//      episode rows, remaining item_file edges, play_state, and chapters).
		stmts := []string{
			"DELETE FROM file WHERE id IN (SELECT file_id FROM item_file WHERE item_id IN (" + epItems + "))",
			"DELETE FROM transcript_fts WHERE episode_id IN (" + epItems + ")",
			"DELETE FROM search_fts WHERE rowid IN (" + epItems + ")",
			"DELETE FROM playable_item WHERE id IN (" + epItems + ")",
		}
		for _, stmt := range stmts {
			if _, err := tx.ExecContext(ctx, stmt, podcastID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}

		// 7. Drop the podcast row and emit its delta. Orphaned podcast/episode art_map
		//    rows are reclaimed by GCArt (the same path as any deleted entity's art).
		if _, err := tx.ExecContext(ctx, "DELETE FROM podcast WHERE id = ?", podcastID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return appendChange(ctx, tx, "podcast", podcastPID, model.OpDelete)
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// SetPodcastRetention updates a podcast's retention policy (keep newest N
// downloaded episodes; 0 keeps all).
func (s *Store) SetPodcastRetention(ctx context.Context, podcastPID model.PID, keep int) error {
	return s.updatePodcastField(ctx, podcastPID, "retention_keep", keep, "store.SetPodcastRetention")
}

// SetPodcastAuthUser records the basic-auth username for a private feed (the
// password lives in the secret table).
func (s *Store) SetPodcastAuthUser(ctx context.Context, podcastPID model.PID, user string) error {
	return s.updatePodcastField(ctx, podcastPID, "auth_user", user, "store.SetPodcastAuthUser")
}

func (s *Store) updatePodcastField(ctx context.Context, podcastPID model.PID, col string, val any, op string) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx,
			"UPDATE podcast SET "+col+" = ?, updated_at = ? WHERE pid = ?", val, nowNS(), string(podcastPID))
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return waxerr.New(waxerr.CodeNotFound, op, "no such podcast: "+string(podcastPID))
		}
		return appendChange(ctx, tx, "podcast", podcastPID, model.OpUpdate)
	})
}

// --- helpers ---------------------------------------------------------------

func episodeTypeOr(t model.EpisodeType) model.EpisodeType {
	if t == "" {
		return model.EpisodeFull
	}
	return t
}

// deletePrimaryFileTx removes an item's current primary file row (cascading its
// item_file edge), if any. Episodes are single-file, so this is the whole backing
// file; it is a no-op when none is attached.
func deletePrimaryFileTx(ctx context.Context, tx *sql.Tx, itemID int64) error {
	var fileID int64
	err := tx.QueryRowContext(ctx,
		"SELECT file_id FROM item_file WHERE item_id = ? AND role = 'primary' LIMIT 1", itemID).Scan(&fileID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "DELETE FROM file WHERE id = ?", fileID)
	return err
}

// idByPIDTx resolves an entity pid to its rowid within a transaction.
func idByPIDTx(ctx context.Context, tx *sql.Tx, table string, pid model.PID, op string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, "SELECT id FROM "+table+" WHERE pid = ?", string(pid)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, waxerr.New(waxerr.CodeNotFound, op, "no such "+table+": "+string(pid))
	}
	if err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return id, nil
}

// itemIDKindByPIDTx resolves an item pid to its rowid and kind within a transaction.
func (s *Store) itemIDKindByPIDTx(ctx context.Context, tx *sql.Tx, pid model.PID, op string) (int64, string, error) {
	var id int64
	var kind string
	err := tx.QueryRowContext(ctx, "SELECT id, kind FROM playable_item WHERE pid = ?", string(pid)).Scan(&id, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", waxerr.New(waxerr.CodeNotFound, op, "no such item: "+string(pid))
	}
	if err != nil {
		return 0, "", waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return id, kind, nil
}
