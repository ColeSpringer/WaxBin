// Package audit inspects a catalog for quality and integrity problems and reports
// severity-ranked findings: duplicate/split entities, inconsistent metadata,
// missing art/ReplayGain, bad filenames, orphaned sidecars, path conflicts,
// invalid feeds, derived-data drift, and (opt-in) on-disk integrity/corruption.
//
// It reads through its own Store port (implemented by store/sqlite) plus two
// injected filesystem helpers, so it depends only on model and never on SQLite.
// Findings are advisory; the repairs live elsewhere (the merge primitive, db
// verify --fix, re-scan/analyze).
package audit

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/colespringer/waxbin/model"
)

// Store is the read-only persistence port the auditor queries.
type Store interface {
	DuplicateArtists(ctx context.Context) ([]model.DuplicateSet, error)
	DuplicateGenres(ctx context.Context) ([]model.DuplicateSet, error)
	DuplicateAlbums(ctx context.Context) ([]model.DuplicateSet, error)
	SplitAlbums(ctx context.Context) ([]model.SplitAlbum, error)
	InconsistentAlbums(ctx context.Context) ([]model.AlbumIssue, error)
	ItemsMissingArt(ctx context.Context, limit int) ([]model.ItemRef, int, error)
	CountItemsMissingReplayGain(ctx context.Context) (int, error)
	AuditFiles(ctx context.Context) ([]model.AuditFileInfo, error)
	Podcasts(ctx context.Context) ([]*model.Podcast, error)
	DerivedDrift(ctx context.Context) (model.DerivedDrift, error)
}

// Hasher recomputes a file's content hash for the integrity (bitrot) check.
type Hasher func(path string) (string, error)

// AudioProbe attempts to parse a file's audio essence; a non-nil error means the
// file is unreadable/corrupt.
type AudioProbe func(ctx context.Context, path string) error

// Config selects which checks run and tunes sampling.
type Config struct {
	// Only, when non-empty, restricts the run to these checks.
	Only []model.AuditCheck
	// Integrity re-reads every audio file to detect bitrot (content-hash mismatch),
	// files missing on disk, and unreadable/corrupt audio. Off by default (I/O heavy)
	// and only runs when a Hasher/AudioProbe was wired in.
	Integrity bool
	// Sample caps the per-check sample size for list-style findings; 0 uses a default.
	Sample int
}

const defaultSample = 50

// Auditor runs quality checks over a catalog.
type Auditor struct {
	store Store
	hash  Hasher
	probe AudioProbe
	log   *slog.Logger
}

// New builds an Auditor. hash/probe may be nil, disabling the integrity and
// corrupt-audio checks respectively.
func New(store Store, hash Hasher, probe AudioProbe, log *slog.Logger) *Auditor {
	if log == nil {
		log = slog.Default()
	}
	return &Auditor{store: store, hash: hash, probe: probe, log: log}
}

// Report is the audit result: findings ordered most-severe first.
type Report struct {
	Findings []model.AuditFinding
	// FilesChecked is how many files the integrity pass read (0 when it did not run).
	FilesChecked int
}

// Errors returns the number of error-severity findings. A non-zero count fails
// the audit (the CLI exits non-zero).
func (r *Report) Errors() int { return r.countSeverity(model.SeverityError) }

// Warnings returns the number of warn-severity findings.
func (r *Report) Warnings() int { return r.countSeverity(model.SeverityWarn) }

func (r *Report) countSeverity(s model.AuditSeverity) int {
	n := 0
	for _, f := range r.Findings {
		if f.Severity == s {
			n++
		}
	}
	return n
}

// Clean reports whether no error- or warn-severity findings were reported.
func (r *Report) Clean() bool { return r.Errors() == 0 && r.Warnings() == 0 }

// Run executes the selected checks and returns their findings, ordered
// most-severe first.
func (a *Auditor) Run(ctx context.Context, cfg Config) (*Report, error) {
	sample := cfg.Sample
	if sample <= 0 {
		sample = defaultSample
	}
	rep := &Report{}
	add := func(f model.AuditFinding) { rep.Findings = append(rep.Findings, f) }

	if a.runs(cfg, model.CheckDuplicateArtist) {
		if err := a.checkDuplicates(ctx, a.store.DuplicateArtists, model.CheckDuplicateArtist, add); err != nil {
			return nil, err
		}
	}
	if a.runs(cfg, model.CheckDuplicateGenre) {
		if err := a.checkDuplicates(ctx, a.store.DuplicateGenres, model.CheckDuplicateGenre, add); err != nil {
			return nil, err
		}
	}
	if a.runs(cfg, model.CheckDuplicateAlbum) {
		if err := a.checkDuplicates(ctx, a.store.DuplicateAlbums, model.CheckDuplicateAlbum, add); err != nil {
			return nil, err
		}
	}
	if a.runs(cfg, model.CheckSplitAlbum) {
		if err := a.checkSplitAlbums(ctx, add); err != nil {
			return nil, err
		}
	}
	if a.runs(cfg, model.CheckInconsistentMeta) {
		if err := a.checkInconsistentAlbums(ctx, add); err != nil {
			return nil, err
		}
	}
	if a.runs(cfg, model.CheckMissingArt) {
		if err := a.checkMissingArt(ctx, sample, add); err != nil {
			return nil, err
		}
	}
	if a.runs(cfg, model.CheckMissingReplayGain) {
		if err := a.checkMissingReplayGain(ctx, add); err != nil {
			return nil, err
		}
	}
	if a.runs(cfg, model.CheckInvalidFeed) {
		if err := a.checkInvalidFeeds(ctx, add); err != nil {
			return nil, err
		}
	}
	if a.runs(cfg, model.CheckDerivedData) {
		if err := a.checkDerived(ctx, add); err != nil {
			return nil, err
		}
	}

	// The filesystem-facing checks all iterate the file list, so fetch it once.
	if a.needsFiles(cfg) {
		if err := a.checkFiles(ctx, cfg, sample, rep, add); err != nil {
			return nil, err
		}
	}

	sortFindings(rep.Findings)
	return rep, nil
}

// runs reports whether check c should run under cfg.
func (a *Auditor) runs(cfg Config, c model.AuditCheck) bool {
	if len(cfg.Only) > 0 {
		for _, x := range cfg.Only {
			if x == c {
				return true
			}
		}
		return false
	}
	// With no explicit selection, run everything except the heavy file-read checks,
	// which only run when Integrity is requested.
	if c == model.CheckIntegrity || c == model.CheckCorruptAudio {
		return cfg.Integrity
	}
	return true
}

// needsFiles reports whether any enabled check reads the file list.
func (a *Auditor) needsFiles(cfg Config) bool {
	return a.runs(cfg, model.CheckBadFilename) ||
		a.runs(cfg, model.CheckOrphanSidecar) ||
		a.runs(cfg, model.CheckPathConflict) ||
		(a.runs(cfg, model.CheckIntegrity) && a.hash != nil) ||
		(a.runs(cfg, model.CheckCorruptAudio) && a.probe != nil)
}

// checkDuplicates turns duplicate sets into merge-candidate findings, deduping
// sets that name the same members (an MBID and a collation-key match can overlap).
func (a *Auditor) checkDuplicates(ctx context.Context, fetch func(context.Context) ([]model.DuplicateSet, error), check model.AuditCheck, add func(model.AuditFinding)) error {
	sets, err := fetch(ctx)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, set := range sets {
		if len(set.Members) < 2 {
			continue
		}
		pids := make([]model.PID, 0, len(set.Members))
		for _, m := range set.Members {
			pids = append(pids, m.PID)
		}
		key := memberKey(pids)
		if seen[key] {
			continue
		}
		seen[key] = true

		names := make([]string, 0, len(set.Members))
		for _, m := range set.Members {
			names = append(names, m.Name)
		}
		add(model.AuditFinding{
			Check:     check,
			Severity:  model.SeverityWarn,
			Message:   "duplicate " + string(set.EntityType) + " (" + set.Reason + "): " + strings.Join(names, " / ") + "; merge with `waxbin merge`",
			Entities:  pids, // survivor first (store orders by track count)
			MergeType: set.EntityType,
		})
	}
	return nil
}

func (a *Auditor) checkSplitAlbums(ctx context.Context, add func(model.AuditFinding)) error {
	splits, err := a.store.SplitAlbums(ctx)
	if err != nil {
		return err
	}
	for _, s := range splits {
		pids := make([]model.PID, 0, len(s.Albums))
		for _, al := range s.Albums {
			pids = append(pids, al.PID)
		}
		add(model.AuditFinding{
			Check:     model.CheckSplitAlbum,
			Severity:  model.SeverityWarn,
			Message:   "album \"" + s.Title + "\" by " + s.Artist + " is split across " + strconv.Itoa(len(s.Albums)) + " album entities; merge with `waxbin merge album`",
			Entities:  pids,
			MergeType: model.MergeAlbum,
		})
	}
	return nil
}

func (a *Auditor) checkInconsistentAlbums(ctx context.Context, add func(model.AuditFinding)) error {
	issues, err := a.store.InconsistentAlbums(ctx)
	if err != nil {
		return err
	}
	for _, is := range issues {
		add(model.AuditFinding{
			Check:    model.CheckInconsistentMeta,
			Severity: model.SeverityInfo,
			Message:  "album \"" + is.Title + "\": " + is.Problem,
			Entities: []model.PID{is.AlbumPID},
		})
	}
	return nil
}

func (a *Auditor) checkMissingArt(ctx context.Context, sample int, add func(model.AuditFinding)) error {
	items, total, err := a.store.ItemsMissingArt(ctx, sample)
	if err != nil {
		return err
	}
	if total == 0 {
		return nil
	}
	for _, it := range items {
		add(model.AuditFinding{
			Check:    model.CheckMissingArt,
			Severity: model.SeverityInfo,
			Message:  "no cover art: " + it.Title,
			Entities: []model.PID{it.PID},
		})
	}
	if total > len(items) {
		add(model.AuditFinding{
			Check:    model.CheckMissingArt,
			Severity: model.SeverityInfo,
			Message:  strconv.Itoa(total) + " items lack cover art (" + strconv.Itoa(len(items)) + " shown)",
		})
	}
	return nil
}

func (a *Auditor) checkMissingReplayGain(ctx context.Context, add func(model.AuditFinding)) error {
	n, err := a.store.CountItemsMissingReplayGain(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		add(model.AuditFinding{
			Check:    model.CheckMissingReplayGain,
			Severity: model.SeverityInfo,
			Message:  strconv.Itoa(n) + " audio files have no ReplayGain; run `waxbin analyze`",
		})
	}
	return nil
}

func (a *Auditor) checkDerived(ctx context.Context, add func(model.AuditFinding)) error {
	d, err := a.store.DerivedDrift(ctx)
	if err != nil {
		return err
	}
	if d.Consistent() {
		return nil
	}
	parts := driftParts(d)
	add(model.AuditFinding{
		Check:    model.CheckDerivedData,
		Severity: model.SeverityError,
		Message:  "derived data is inconsistent (" + strings.Join(parts, ", ") + "); repair with `waxbin db verify --fix`",
	})
	return nil
}

func driftParts(d model.DerivedDrift) []string {
	var p []string
	addPart := func(n int, label string) {
		if n > 0 {
			p = append(p, strconv.Itoa(n)+" "+label)
		}
	}
	addPart(d.ItemsMissingFTS, "items missing FTS")
	addPart(d.OrphanFTSRows, "orphan FTS rows")
	addPart(d.ArtistRollupDrift, "artist rollup")
	addPart(d.GenreRollupDrift, "genre rollup")
	addPart(d.ReleaseGroupRollupDrift, "release-group rollup")
	addPart(d.SortKeyDrift, "sort-key")
	addPart(d.BookDurationDrift, "book-duration")
	return p
}

// memberKey is a stable key over a set of PIDs, for deduping duplicate findings.
func memberKey(pids []model.PID) string {
	s := make([]string, len(pids))
	for i, p := range pids {
		s[i] = string(p)
	}
	sort.Strings(s)
	return strings.Join(s, "\x1f")
}

// severityRank orders findings error > warn > info.
func severityRank(s model.AuditSeverity) int {
	switch s {
	case model.SeverityError:
		return 0
	case model.SeverityWarn:
		return 1
	default:
		return 2
	}
}

func sortFindings(fs []model.AuditFinding) {
	sort.SliceStable(fs, func(i, j int) bool {
		if severityRank(fs[i].Severity) != severityRank(fs[j].Severity) {
			return severityRank(fs[i].Severity) < severityRank(fs[j].Severity)
		}
		return fs[i].Check < fs[j].Check
	})
}
