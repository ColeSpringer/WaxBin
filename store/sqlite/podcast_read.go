package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// podcastSelect reads a podcast row plus its episode and downloaded counts.
const podcastSelect = `SELECT p.id, p.pid, p.feed_url, p.identity_key, p.title, p.sort_key, p.author,
	p.description, p.link, p.language, p.category, p.explicit,
	p.funding_url, p.funding_message, p.medium, p.image_url, p.guid, p.etag,
	p.last_modified, p.last_fetched_at, p.retention_keep, p.auth_user, p.source_type, p.created_at, p.updated_at,
	(SELECT COUNT(*) FROM episode e WHERE e.podcast_id = p.id) AS ep_count,
	(SELECT COUNT(*) FROM episode e JOIN playable_item pi ON pi.id = e.item_id
	   WHERE e.podcast_id = p.id AND pi.state = 'present') AS dl_count
	FROM podcast p`

func scanPodcast(sc rowScanner) (*model.Podcast, error) {
	var p model.Podcast
	var lastFetched sql.NullInt64
	if err := sc.Scan(&p.ID, &p.PID, &p.FeedURL, &p.IdentityKey, &p.Title, &p.SortKey, &p.Author,
		&p.Description, &p.Link, &p.Language, &p.Category, &p.Explicit,
		&p.FundingURL, &p.FundingMessage, &p.Medium, &p.ImageURL, &p.GUID, &p.ETag,
		&p.LastModified, &lastFetched, &p.RetentionKeep, &p.AuthUser, &p.SourceType, &p.CreatedAt, &p.UpdatedAt,
		&p.EpisodeCount, &p.DownloadedCount); err != nil {
		return nil, err
	}
	p.LastFetchedAt = lastFetched.Int64
	return &p, nil
}

// Podcasts lists subscribed podcasts, sorted by title.
func (s *Store) Podcasts(ctx context.Context) ([]*model.Podcast, error) {
	const op = "store.Podcasts"
	rows, err := s.read.QueryContext(ctx, podcastSelect+" ORDER BY p.sort_key, p.pid")
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []*model.Podcast
	for rows.Next() {
		p, err := scanPodcast(rows)
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PodcastByPID returns one podcast by public id, with its channel-level person
// credits (a detail-only load; Podcasts, the list read, leaves them empty).
func (s *Store) PodcastByPID(ctx context.Context, pid model.PID) (*model.Podcast, error) {
	const op = "store.PodcastByPID"
	p, err := scanPodcast(s.read.QueryRowContext(ctx, podcastSelect+" WHERE p.pid = ?", string(pid)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, op, "no such podcast: "+string(pid))
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	persons, err := queryPersons(ctx, s.read,
		"SELECT name, role, grp, img, href FROM podcast_person WHERE podcast_id = ? AND item_id IS NULL ORDER BY position",
		p.ID)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	p.Persons = persons
	return p, nil
}

// queryPersons runs a person listing (name, role, grp, img, href columns, in
// that order) and scans it into feed credits. The cursor is closed when it
// returns, so a transactional caller is free to write on the same connection
// afterward (the chaptersInSync pattern).
func queryPersons(ctx context.Context, q queryer, stmt string, args ...any) ([]model.FeedPerson, error) {
	rows, err := q.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.FeedPerson
	for rows.Next() {
		var p model.FeedPerson
		if err := rows.Scan(&p.Name, &p.Role, &p.Group, &p.Img, &p.Href); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// querySoundbites is queryPersons for soundbite rows (start_ms, duration_ms,
// title columns).
func querySoundbites(ctx context.Context, q queryer, stmt string, args ...any) ([]model.FeedSoundbite, error) {
	rows, err := q.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.FeedSoundbite
	for rows.Next() {
		var b model.FeedSoundbite
		if err := rows.Scan(&b.StartMS, &b.DurationMS, &b.Title); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// PodcastByIdentity returns the podcast with the given identity key, or
// CodeNotFound. The sync path uses it to load the prior ETag/Last-Modified
// validators, retention policy, and stored auth user before re-fetching.
func (s *Store) PodcastByIdentity(ctx context.Context, key string) (*model.Podcast, error) {
	const op = "store.PodcastByIdentity"
	p, err := scanPodcast(s.read.QueryRowContext(ctx, podcastSelect+" WHERE p.identity_key = ?", key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, op, "no podcast with that identity")
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return p, nil
}

// episodeSelect reads an episode joined to its podcast and (when downloaded) its
// primary file, plus whether a transcript is stored.
const episodeSelect = `SELECT pi.pid, pi.title, pi.state, p.pid, p.title,
	e.guid, e.description, e.link, e.pub_date, e.year, e.season, e.episode_no, e.episode_type,
	e.duration_ms, e.explicit, e.enclosure_url, e.enclosure_type, e.enclosure_size,
	e.transcript_url, e.transcript_type, e.chapters_url, e.image_url, e.pinned, e.created_at, e.updated_at,
	f.pid, f.display_path, f.duration_ms,
	EXISTS(SELECT 1 FROM episode_transcript et WHERE et.item_id = pi.id) AS has_transcript
	FROM episode e
	JOIN playable_item pi ON pi.id = e.item_id
	JOIN podcast p ON p.id = e.podcast_id
	LEFT JOIN item_file pf ON pf.item_id = pi.id AND pf.role = 'primary'
	LEFT JOIN file f ON f.id = pf.file_id`

func scanEpisode(sc rowScanner) (*model.Episode, bool, error) {
	var e model.Episode
	var state, epType string
	var pubDate, year, season, episodeNo, durMS, fileDurMS sql.NullInt64
	var fpid, fdisp sql.NullString
	var hasTranscript bool
	if err := sc.Scan(&e.PID, &e.Title, &state, &e.PodcastPID, &e.PodcastTitle,
		&e.GUID, &e.Description, &e.Link, &pubDate, &year, &season, &episodeNo, &epType,
		&durMS, &e.Explicit, &e.EnclosureURL, &e.EnclosureType, &e.EnclosureSize,
		&e.TranscriptURL, &e.TranscriptType, &e.ChaptersURL, &e.ImageURL, &e.Pinned, &e.CreatedAt, &e.UpdatedAt,
		&fpid, &fdisp, &fileDurMS, &hasTranscript); err != nil {
		return nil, false, err
	}
	e.State = model.ItemState(state)
	e.EpisodeType = model.EpisodeType(epType)
	e.PubDateNS = pubDate.Int64
	e.Year = int(year.Int64)
	e.Season = int(season.Int64)
	e.EpisodeNo = int(episodeNo.Int64)
	// Prefer the actual file duration once downloaded, else the feed-declared value.
	e.DurationMS = durMS.Int64
	if fileDurMS.Valid && fileDurMS.Int64 > 0 {
		e.DurationMS = fileDurMS.Int64
	}
	e.FilePID = model.PID(fpid.String)
	e.DisplayPath = fdisp.String
	e.Downloaded = e.State == model.StatePresent && fpid.Valid
	return &e, hasTranscript, nil
}

// EpisodesByPodcast lists a podcast's episodes, newest publication first (undated
// last). limit 0 returns all.
func (s *Store) EpisodesByPodcast(ctx context.Context, podcastPID model.PID, limit int) ([]*model.Episode, error) {
	const op = "store.EpisodesByPodcast"
	// Tie-break pi.pid ASC (like DownloadedEpisodes): a feed lists newest-first and is
	// ingested in that order, so within a same-pubdate batch the newest episode holds the
	// lowest ULID; ASC lists it first, honoring the newest-first contract.
	stmt := episodeSelect + " WHERE p.pid = ? ORDER BY COALESCE(e.pub_date, 0) DESC, pi.pid ASC"
	args := []any{string(podcastPID)}
	if limit > 0 {
		stmt += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.read.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []*model.Episode
	for rows.Next() {
		e, _, err := scanEpisode(rows)
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EpisodeByPID returns one episode by public id, with HasTranscript set.
// EpisodeChapters returns a downloaded episode's chapters in timeline order,
// preferring the podcast:chapters JSON source over any embedded chapters. It reuses
// the book chapter timeline over the episode's single backing file. An episode with
// no downloaded file or no chapters returns an empty slice.
func (s *Store) EpisodeChapters(ctx context.Context, pid model.PID) ([]model.Chapter, error) {
	const op = "store.EpisodeChapters"
	var itemID, fileID, dur int64
	var fpid string
	err := s.read.QueryRowContext(ctx, `SELECT pi.id, f.id, f.pid, COALESCE(f.duration_ms, 0)
		FROM playable_item pi
		JOIN item_file pf ON pf.item_id = pi.id AND pf.role = 'primary'
		JOIN file f ON f.id = pf.file_id
		WHERE pi.pid = ? AND pi.kind = 'episode'`, string(pid)).Scan(&itemID, &fileID, &fpid, &dur)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil // not downloaded (no primary file), so no chapters
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	part := bookPart{BookPart: model.BookPart{FilePID: model.PID(fpid), DurationMS: dur}, fileID: fileID}
	chs, _, err := s.bookChapters(ctx, itemID, []bookPart{part})
	return chs, err
}

func (s *Store) EpisodeByPID(ctx context.Context, pid model.PID) (*model.EpisodeDetail, error) {
	const op = "store.EpisodeByPID"
	e, hasTranscript, err := scanEpisode(s.read.QueryRowContext(ctx, episodeSelect+" WHERE pi.pid = ?", string(pid)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, op, "no such episode: "+string(pid))
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	chapters, err := s.EpisodeChapters(ctx, pid)
	if err != nil {
		return nil, err
	}
	d := &model.EpisodeDetail{Episode: e, HasTranscript: hasTranscript, Chapters: chapters}

	// The Podcasting 2.0 extras are detail-only loads, keeping the list reads to
	// their single query.
	d.Persons, err = queryPersons(ctx, s.read, `SELECT pp.name, pp.role, pp.grp, pp.img, pp.href
		FROM podcast_person pp JOIN playable_item pi ON pi.id = pp.item_id
		WHERE pi.pid = ? ORDER BY pp.position`, string(pid))
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	d.Soundbites, err = querySoundbites(ctx, s.read, `SELECT sb.start_ms, sb.duration_ms, sb.title
		FROM episode_soundbite sb JOIN playable_item pi ON pi.id = sb.item_id
		WHERE pi.pid = ? ORDER BY sb.position`, string(pid))
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return d, nil
}

// TranscriptByEpisode returns an episode's stored transcript, or CodeNotFound
// when the episode exists but no transcript is stored (a missing episode is its
// own CodeNotFound, so the two absences read differently).
func (s *Store) TranscriptByEpisode(ctx context.Context, pid model.PID) (*model.Transcript, error) {
	const op = "store.TranscriptByEpisode"
	itemID, kind, err := s.itemIDKindByPID(ctx, pid, op)
	if err != nil {
		return nil, err
	}
	if kind != string(model.KindEpisode) {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "item is not an episode: "+string(pid))
	}
	tr := &model.Transcript{EpisodePID: pid}
	err = s.read.QueryRowContext(ctx,
		"SELECT format, body, source_url, created_at FROM episode_transcript WHERE item_id = ?", itemID).
		Scan(&tr.Format, &tr.Body, &tr.SourceURL, &tr.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, op, "no transcript stored for episode "+string(pid))
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return tr, nil
}

// DownloadedEpisodes lists a podcast's currently-downloaded episodes eligible for
// retention, newest first, so retention can keep the newest N and drop the rest.
// Each carries its file's display path for on-disk removal. Pinned episodes are
// excluded entirely: retention never counts them toward N nor reclaims them.
//
// The pub_date tie-break is pid ASC, not DESC: a feed lists newest-first and the
// upsert loop ingests in that order, so within a same-date batch the newest episode
// holds the lowest (earliest) ULID. Ordering the tie ascending puts it first (kept),
// so "keep newest N" never reclaims the newest same-date file and keeps an older one.
func (s *Store) DownloadedEpisodes(ctx context.Context, podcastPID model.PID) ([]*model.Episode, error) {
	const op = "store.DownloadedEpisodes"
	rows, err := s.read.QueryContext(ctx,
		episodeSelect+" WHERE p.pid = ? AND pi.state = 'present' AND COALESCE(e.pinned,0) = 0"+
			" ORDER BY COALESCE(e.pub_date, 0) DESC, pi.pid ASC",
		string(podcastPID))
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []*model.Episode
	for rows.Next() {
		e, _, err := scanEpisode(rows)
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
