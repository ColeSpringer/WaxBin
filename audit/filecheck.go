package audit

import (
	"context"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/organize"
)

// checkInvalidFeeds flags podcast shows whose feed looks broken: an rss/youtube
// show with an unparseable feed URL, or a synced show with no episodes.
func (a *Auditor) checkInvalidFeeds(ctx context.Context, add func(model.AuditFinding)) error {
	pods, err := a.store.Podcasts(ctx)
	if err != nil {
		return err
	}
	for _, p := range pods {
		src := p.SourceType
		if src == "" {
			src = model.SourceRSS
		}
		// Only RSS feeds carry an HTTP(S) feed URL. A manual show uses a synthetic
		// "manual:<ulid>" key, and an injected provider (e.g. youtube) stores a
		// provider-specific URL in feed_url, so validating either as an HTTP feed
		// would false-positive a perfectly valid subscription.
		if src == model.SourceRSS {
			if u, err := url.Parse(p.FeedURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
				add(model.AuditFinding{
					Check:    model.CheckInvalidFeed,
					Severity: model.SeverityWarn,
					Message:  "podcast \"" + p.Title + "\" has an invalid feed URL: " + p.FeedURL,
					Entities: []model.PID{p.PID},
				})
				continue
			}
		}
		if p.EpisodeCount == 0 {
			add(model.AuditFinding{
				Check:    model.CheckInvalidFeed,
				Severity: model.SeverityInfo,
				Message:  "podcast \"" + p.Title + "\" has no episodes",
				Entities: []model.PID{p.PID},
			})
		}
	}
	return nil
}

// checkFiles runs the file-list-driven checks in one pass: bad filenames, orphaned
// sidecars, case-insensitive path conflicts, and (opt-in) on-disk integrity and
// corrupt-audio detection.
func (a *Auditor) checkFiles(ctx context.Context, cfg Config, sample int, rep *Report, add func(model.AuditFinding), corruptSeen map[string]bool) error {
	files, err := a.store.AuditFiles(ctx)
	if err != nil {
		return err
	}

	// Directories that hold at least one audio file, for orphan-sidecar detection.
	audioDirs := map[string]bool{}
	for _, f := range files {
		if f.Kind == model.FileAudio {
			audioDirs[filepath.Dir(string(f.Path))] = true
		}
	}

	doBad := a.runs(cfg, model.CheckBadFilename)
	doOrphan := a.runs(cfg, model.CheckOrphanSidecar)
	doConflict := a.runs(cfg, model.CheckPathConflict)

	bad := &capped{limit: sample, check: model.CheckBadFilename, sev: model.SeverityWarn, add: add}
	orphan := &capped{limit: sample, check: model.CheckOrphanSidecar, sev: model.SeverityWarn, add: add}
	foldGroups := map[string][]model.AuditFileInfo{}

	for _, f := range files {
		if doConflict {
			key := strings.ToLower(string(f.Path))
			foldGroups[key] = append(foldGroups[key], f)
		}
		if doBad {
			if reason := badFilename(f); reason != "" {
				bad.emit(model.AuditFinding{
					Check:    model.CheckBadFilename,
					Severity: model.SeverityWarn,
					Message:  "unportable filename (" + reason + "): " + f.DisplayPath,
					Path:     f.DisplayPath,
				})
			}
		}
		if doOrphan && f.Kind != model.FileAudio && f.Kind != model.FileForeign {
			if !audioDirs[filepath.Dir(string(f.Path))] {
				orphan.emit(model.AuditFinding{
					Check:    model.CheckOrphanSidecar,
					Severity: model.SeverityWarn,
					Message:  "orphaned " + string(f.Kind) + " sidecar (no audio in its folder): " + f.DisplayPath,
					Path:     f.DisplayPath,
				})
			}
		}
	}
	bad.summary("unportable filenames")
	orphan.summary("orphaned sidecars")

	if doConflict {
		a.reportPathConflicts(foldGroups, sample, add)
	}

	if a.runs(cfg, model.CheckIntegrity) && a.hash != nil {
		if err := a.checkIntegrity(ctx, files, sample, rep, add); err != nil {
			return err
		}
	}
	// The probe half only: the cheap diagnostic half runs outside checkFiles, since it
	// reads no files.
	if a.runs(cfg, model.CheckCorruptAudio) && cfg.Integrity && a.probe != nil {
		if err := a.checkCorrupt(ctx, files, sample, add, corruptSeen); err != nil {
			return err
		}
	}
	return nil
}

// reportPathConflicts emits a finding per set of files whose paths differ only by
// case (a collision on a case-insensitive filesystem). Groups are sorted for
// deterministic output.
func (a *Auditor) reportPathConflicts(groups map[string][]model.AuditFileInfo, sample int, add func(model.AuditFinding)) {
	keys := make([]string, 0, len(groups))
	for k, g := range groups {
		if len(g) > 1 {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	c := &capped{limit: sample, check: model.CheckPathConflict, sev: model.SeverityError, add: add}
	for _, k := range keys {
		g := groups[k]
		names := make([]string, 0, len(g))
		pids := make([]model.PID, 0, len(g))
		for _, f := range g {
			names = append(names, f.DisplayPath)
			pids = append(pids, f.PID)
		}
		c.emit(model.AuditFinding{
			Check:    model.CheckPathConflict,
			Severity: model.SeverityError,
			Message:  "case-insensitive path collision: " + strings.Join(names, " vs "),
			Entities: pids,
		})
	}
	c.summary("path collisions")
}

// checkIntegrity re-hashes each audio file and flags a content-hash mismatch
// (bitrot or external edit) or a file missing on disk.
func (a *Auditor) checkIntegrity(ctx context.Context, files []model.AuditFileInfo, sample int, rep *Report, add func(model.AuditFinding)) error {
	c := &capped{limit: sample, check: model.CheckIntegrity, sev: model.SeverityError, add: add}
	for _, f := range files {
		if f.Kind != model.FileAudio {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rep.FilesChecked++
		got, err := a.hash(pathx.Long(string(f.Path)))
		if err != nil {
			c.emit(model.AuditFinding{
				Check:    model.CheckIntegrity,
				Severity: model.SeverityError,
				Message:  "unreadable or missing on disk: " + f.DisplayPath,
				Path:     f.DisplayPath,
				Entities: nonEmpty(f.ItemPID),
			})
			continue
		}
		if f.ContentHash != "" && got != f.ContentHash {
			c.emit(model.AuditFinding{
				Check:    model.CheckIntegrity,
				Severity: model.SeverityError,
				Message:  "content hash changed (possible bitrot or external edit): " + f.DisplayPath,
				Path:     f.DisplayPath,
				Entities: nonEmpty(f.ItemPID),
			})
		}
	}
	c.summary("integrity failures")
	return nil
}

// checkCorrupt probes each audio file's essence and flags one that fails to parse.
func (a *Auditor) checkCorrupt(ctx context.Context, files []model.AuditFileInfo, sample int, add func(model.AuditFinding), seen map[string]bool) error {
	c := &capped{limit: sample, check: model.CheckCorruptAudio, sev: model.SeverityError, add: add}
	for _, f := range files {
		if f.Kind != model.FileAudio {
			continue
		}
		// Already reported by this check's cheap half, which read the diagnostic the
		// scan derived. Both halves flagging one file must still be one finding.
		if seen[f.DisplayPath] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := a.probe(ctx, pathx.Long(string(f.Path))); err != nil {
			c.emit(model.AuditFinding{
				Check:    model.CheckCorruptAudio,
				Severity: model.SeverityError,
				Message:  "corrupt or undecodable audio: " + f.DisplayPath,
				Path:     f.DisplayPath,
				Entities: nonEmpty(f.ItemPID),
			})
		}
	}
	c.summary("corrupt files")
	return nil
}

// badFilename reports why a file's name is not portable, or "" when it is fine.
// It checks the filename segment against the same portability rules organize
// enforces on write, plus UTF-8 validity of the raw bytes.
func badFilename(f model.AuditFileInfo) string {
	base := filepath.Base(string(f.Path))
	if !utf8.ValidString(base) {
		return "non-UTF-8 filename"
	}
	return organize.UnsafeSegmentReason(base)
}

func nonEmpty(pid model.PID) []model.PID {
	if pid == "" {
		return nil
	}
	return []model.PID{pid}
}

// capped emits up to limit findings and counts the rest, so a pervasive problem
// yields a bounded list plus a "N more" summary rather than flooding the report.
type capped struct {
	limit, shown, total int
	check               model.AuditCheck
	sev                 model.AuditSeverity
	add                 func(model.AuditFinding)
}

func (c *capped) emit(f model.AuditFinding) {
	c.total++
	if c.shown < c.limit {
		c.add(f)
		c.shown++
	}
}

func (c *capped) summary(noun string) {
	if c.total > c.shown {
		c.add(model.AuditFinding{
			Check:    c.check,
			Severity: c.sev,
			Message:  strconv.Itoa(c.total) + " " + noun + " (" + strconv.Itoa(c.shown) + " shown)",
		})
	}
}
