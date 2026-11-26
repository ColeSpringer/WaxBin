package waxbin

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/colespringer/waxbin/jobs"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/scan"
	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/colespringer/waxbin/waxerr"
)

// BookPartChange is one part AssignBookParts files: FilePID's file into the book BookPID
// names, at Disc and Part (model.PartPosition), or with both 0 at the number the file's own
// tags give it. An empty BookPID withdraws the file's assignment.
type BookPartChange struct {
	FilePID model.PID `json:"filePid"`
	BookPID model.PID `json:"bookPid,omitempty"`
	Disc    int       `json:"disc,omitempty"`
	Part    int       `json:"part,omitempty"`
}

// BookPartsReport is what AssignBookParts did: the item each file it read backs now, its
// copies included (Files), each item a file left that folded into another, with its plays,
// stars and the rest (Absorbed), each item a read made, mapped to the item its file came from
// (Created), and the files it withdrew an assignment from (Withdrawn).
type BookPartsReport struct {
	Files    map[model.PID]model.PID `json:"files,omitempty"`
	Absorbed map[model.PID]model.PID `json:"absorbed,omitempty"`
	Created  map[model.PID]model.PID `json:"created,omitempty"`
	// Withdrawn lists the files whose audio a change took the assignment from.
	Withdrawn []model.PID `json:"withdrawn,omitempty"`
}

// The reasons AssignBookParts refuses a change, each beside its refusal's class.
const (
	ReasonBookPartNotBook    waxerr.Reason = "book_part_not_book"    // CodeInvalid: the target is no book
	ReasonBookPartEpisode    waxerr.Reason = "book_part_episode"     // CodeInvalid
	ReasonBookPartShared     waxerr.Reason = "book_part_shared"      // CodeInvalid: several items, or a cue rip's tracks
	ReasonBookPartNotOnDisk  waxerr.Reason = "book_part_not_on_disk" // CodeConflict
	ReasonBookPartNoBooks    waxerr.Reason = "book_part_no_books"    // CodeInvalid: the file's library takes no books
	ReasonBookPartLocked     waxerr.Reason = model.ReasonKindLocked  // CodeLocked: a track whose kind is locked
	ReasonBookPartPlaceTaken waxerr.Reason = "book_part_place_taken" // CodeInvalid: two changes give one place
	// CodeInvalid: the book's parts sit in another library than the file
	ReasonBookPartOtherLibrary waxerr.Reason = "book_part_other_library"
)

// AssignBookParts files book parts by what a host assigns rather than by their tags, under a
// "book-parts" job on the file lease. Each change's file is read again into the book it names
// at the place it gives, and the catalog keeps the assignment: later reads file the file
// there whatever its tags say, the book keeping the title, author and identifiers the catalog
// holds, until the assignment is withdrawn, the file is read as a track by force
// (SetItemKind), or the book goes. It files a part into a standing book, renumbers one
// already there, and gathers the one-part books an earlier import left into one: name one as
// the book and assign the others' files to it, and each folds into it. A change naming no
// book withdraws the assignment of the file's audio and files the file as every later scan
// will, by its own tags and the folder rule, in the same write; a file whose audio has no
// assignment is left as it is. The audio's other files (its copies) are read again with the
// file, so the whole audio moves at once.
//
// Refused before anything changes: no changes, a file named twice, a number past a disc's
// places, a target that is no book, a file that is an episode's, backs several items or a cue
// rip's tracks, or is not on disk, a file assigned in a library that takes no books, to a book
// whose parts sit in another library, or backing a track whose kind is locked, and two changes
// giving one place. A read that fails partway fails the job, and the
// report returned with the error says what it did before it. StartAssignBookParts is the
// asynchronous variant a server exposes.
func (l *Library) AssignBookParts(ctx context.Context, changes []BookPartChange) (*BookPartsReport, error) {
	const op = "waxbin.AssignBookParts"
	if _, err := l.bookPartReads(ctx, op, changes); err != nil {
		return nil, err
	}
	var rep *BookPartsReport
	_, err := l.jobs.Run(ctx, bookPartsSpec(changes), func(jctx context.Context, h *jobs.Handle) error {
		var err error
		rep, err = l.bookPartsWork(jctx, op, changes, h)
		return err
	})
	return rep, err
}

// StartAssignBookParts submits AssignBookParts as a background job and returns its pid; the
// job's Result holds the BookPartsReport as JSON. The changes are checked before the job
// starts, so a refused one starts none.
func (l *Library) StartAssignBookParts(ctx context.Context, changes []BookPartChange) (model.PID, error) {
	const op = "waxbin.StartAssignBookParts"
	if _, err := l.bookPartReads(ctx, op, changes); err != nil {
		return "", err
	}
	changes = slices.Clone(changes)
	return l.startJob(ctx, bookPartsSpec(changes), func(jctx context.Context, h *jobs.Handle) error {
		_, err := l.bookPartsWork(jctx, op, changes, h)
		return err
	})
}

// bookPartsSpec names a book-parts job, targeting the book when every change names the same.
func bookPartsSpec(changes []BookPartChange) jobs.Spec {
	spec := jobs.Spec{Kind: "book-parts", Scope: fsMutateScope}
	if len(changes) > 0 && changes[0].BookPID != "" && !slices.ContainsFunc(changes, func(c BookPartChange) bool {
		return c.BookPID != changes[0].BookPID
	}) {
		spec.TargetType, spec.TargetPID = "item", changes[0].BookPID
	}
	return spec
}

// bookPartRead is a change as AssignBookParts reads it: the file, its library, and the place.
type bookPartRead struct {
	BookPartChange
	file     sqlite.PartFile
	lib      *model.Library
	position *int
}

// bookPartReads checks the changes (see AssignBookParts) and returns them as they are read.
func (l *Library) bookPartReads(ctx context.Context, op string, changes []BookPartChange) ([]bookPartRead, error) {
	if l.ReadOnly() {
		return nil, waxerr.New(waxerr.CodeUnsupported, op, "filing book parts requires a read-write library")
	}
	if len(changes) == 0 {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "no parts named")
	}
	pids := make([]model.PID, len(changes))
	byFile := make(map[model.PID]BookPartChange, len(changes))
	for i, c := range changes {
		if slices.Contains(pids[:i], c.FilePID) {
			return nil, waxerr.New(waxerr.CodeInvalid, op, "file "+string(c.FilePID)+" is named twice")
		}
		pids[i], byFile[c.FilePID] = c.FilePID, c
	}
	files, err := l.store.PartFiles(ctx, pids)
	if err != nil {
		return nil, err
	}
	libs, err := l.store.Libraries(ctx)
	if err != nil {
		return nil, err
	}
	books := map[model.PID][]model.ItemFileRef{} // each target book's parts, read once
	places := map[model.PID]map[int]string{}     // the places the changes give each book, to the audio given each
	reads := make([]bookPartRead, len(changes))
	for i, c := range changes {
		f := files[i]
		r := bookPartRead{BookPartChange: c, file: f, lib: libraryByID(libs, f.LibraryID)}
		switch {
		case c.Disc < 0 || c.Part < 0 || c.Part >= model.DiscStride || (c.Disc > 0 && c.Part == 0):
			return nil, waxerr.New(waxerr.CodeInvalid, op, fmt.Sprintf("disc %d part %d is no part's place (parts number from 1, below %d)",
				c.Disc, c.Part, model.DiscStride))
		case c.BookPID == "" && (c.Disc != 0 || c.Part != 0):
			return nil, waxerr.New(waxerr.CodeInvalid, op, "a withdrawal takes no number")
		}
		// One audio takes one place, so two of its files cannot be sent two.
		for j, g := range files[:i] {
			if d := changes[j]; g.Essence != "" && g.Essence == f.Essence && (d.BookPID != c.BookPID || d.Disc != c.Disc || d.Part != c.Part) {
				return nil, waxerr.New(waxerr.CodeInvalid, op, g.DisplayPath+" and "+f.DisplayPath+" hold the same audio, which takes one place")
			}
		}
		if c.Part > 0 {
			p := model.PartPosition(c.Disc, c.Part)
			if e, ok := places[c.BookPID][p]; ok && (e == "" || e != f.Essence) {
				return nil, waxerr.New(waxerr.CodeInvalid, op, fmt.Sprintf("two parts are given disc %d part %d of book %s", c.Disc, c.Part, c.BookPID)).
					WithReason(ReasonBookPartPlaceTaken)
			}
			if places[c.BookPID] == nil {
				places[c.BookPID] = map[int]string{}
			}
			places[c.BookPID][p] = f.Essence
			r.position = &p
		}
		if err := partRefusal(op, f, r.lib, c.BookPID != ""); err != nil {
			return nil, err
		}
		if c.BookPID != "" {
			parts, ok := books[c.BookPID]
			if !ok {
				v, err := l.store.ItemByPID(ctx, c.BookPID)
				if err != nil {
					return nil, err
				}
				if v.Kind != model.KindBook {
					return nil, waxerr.New(waxerr.CodeInvalid, op, fmt.Sprintf("item %s is a %s, not a book", c.BookPID, v.Kind)).
						WithReason(ReasonBookPartNotBook)
				}
				if parts, err = l.store.ItemFiles(ctx, c.BookPID); err != nil {
					return nil, err
				}
				parts = slices.DeleteFunc(parts, func(p model.ItemFileRef) bool { return p.Role == "alternate" })
				books[c.BookPID] = parts
			}
			if slices.ContainsFunc(parts, func(p model.ItemFileRef) bool { return p.LibraryPID != r.lib.PID }) {
				return nil, waxerr.New(waxerr.CodeInvalid, op, fmt.Sprintf("book %s has parts in another library than %s", c.BookPID, f.DisplayPath)).
					WithReason(ReasonBookPartOtherLibrary)
			}
			if r.position != nil {
				i := slices.IndexFunc(parts, func(p model.ItemFileRef) bool { return p.Position == *r.position && p.FilePID != f.FilePID })
				if i >= 0 && !movesOff(byFile, parts[i].FilePID, c.BookPID, *r.position) {
					// Another file of the same audio holding the place is this part.
					holder, err := l.store.FileByPID(ctx, parts[i].FilePID)
					if err != nil {
						return nil, err
					}
					if holder.EssenceHash == "" || holder.EssenceHash != f.Essence {
						return nil, waxerr.New(waxerr.CodeInvalid, op, fmt.Sprintf("disc %d part %d of book %s is %s", c.Disc, c.Part, c.BookPID,
							parts[i].DisplayPath)).WithReason(ReasonBookPartPlaceTaken)
					}
				}
			}
		}
		present, err := onDisk(string(f.Path))
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if !present {
			return nil, waxerr.New(waxerr.CodeConflict, op, f.DisplayPath+" is not on disk").WithReason(ReasonBookPartNotOnDisk)
		}
		reads[i] = r
	}
	return reads, nil
}

// partRefusal says why a file cannot be a book part, nil when it can. filing says the change
// files it into a book; a withdrawal makes nothing a book, so it is not held to what one needs.
func partRefusal(op string, f sqlite.PartFile, lib *model.Library, filing bool) error {
	switch {
	case f.FileKind != model.FileAudio:
		return waxerr.New(waxerr.CodeInvalid, op, f.DisplayPath+" is no audio file")
	case f.Kind == model.KindEpisode:
		return waxerr.New(waxerr.CodeInvalid, op, f.DisplayPath+" is an episode's file").WithReason(ReasonBookPartEpisode)
	case f.Items > 1 || f.Windowed:
		return waxerr.New(waxerr.CodeInvalid, op, f.DisplayPath+" backs several items or a cue rip's tracks").
			WithReason(ReasonBookPartShared)
	case lib == nil:
		return waxerr.New(waxerr.CodeInternal, op, "no library holds "+f.DisplayPath)
	case filing && !lib.TakesBooks():
		return waxerr.New(waxerr.CodeInvalid, op, f.DisplayPath+" is in a library that takes no books").
			WithReason(ReasonBookPartNoBooks)
	case filing && f.Kind == model.KindTrack && f.KindLocked:
		return waxerr.New(waxerr.CodeLocked, op, fmt.Sprintf("item %s, which %s backs, is locked as a track", f.ItemPID, f.DisplayPath)).
			WithReason(ReasonBookPartLocked)
	}
	return nil
}

// movesOff reports whether the change assigns the part on file elsewhere: into another book
// or to another place in book. A withdrawal does not count, since the file's own tags may
// put it back in the same place.
func movesOff(byFile map[model.PID]BookPartChange, file, book model.PID, place int) bool {
	c, ok := byFile[file]
	if !ok || c.BookPID == "" {
		return false
	}
	return c.BookPID != book || c.Part > 0 && model.PartPosition(c.Disc, c.Part) != place
}

// bookPartsWork files the parts under the job, each read again after the changes are checked
// once more, and records the report as the job's result however the reads end.
func (l *Library) bookPartsWork(ctx context.Context, op string, changes []BookPartChange, h *jobs.Handle) (rep *BookPartsReport, err error) {
	rep = &BookPartsReport{Files: map[model.PID]model.PID{}, Absorbed: map[model.PID]model.PID{}, Created: map[model.PID]model.PID{}}
	defer func() {
		model.SettleIdentities(nil, rep.Absorbed, rep.Created)
		h.SetResult(l.jsonResult(rep))
	}()
	reads, err := l.bookPartReads(ctx, op, changes)
	if err != nil {
		return rep, err
	}
	if err := h.SetParams(ctx, changes); err != nil {
		return rep, err
	}
	libs, err := l.store.Libraries(ctx)
	if err != nil {
		return rep, err
	}
	for i, r := range reads {
		if err := h.Heartbeat(ctx, float64(i)/float64(len(reads)), fmt.Sprintf("filing part %d of %d", i+1, len(reads))); err != nil {
			return rep, err
		}
		opts := scan.FileOptions{Assign: &scan.Assignment{BookPID: r.BookPID, Position: r.position}}
		if r.BookPID == "" {
			ap, err := l.store.AssignedPart(ctx, r.file.Path, r.file.Essence)
			if err != nil {
				return rep, err
			}
			if ap == nil {
				owner, err := l.store.WholeFileOwner(ctx, r.FilePID)
				if err != nil {
					return rep, err
				}
				rep.Files[r.FilePID] = owner
				continue
			}
			opts = scan.FileOptions{Withdraw: true}
		}
		if err := l.readPart(ctx, r.lib, r.FilePID, string(r.file.Path), opts, rep); err != nil {
			return rep, err
		}
		if r.BookPID == "" {
			rep.Withdrawn = append(rep.Withdrawn, r.FilePID)
		}
		if err := l.followAudio(ctx, op, r, libs, rep); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// followAudio reads again the other files of a change's audio, so the whole audio moves with
// the change rather than at each file's next scan. A file the change could not file itself
// (an episode's download, a cue rip's file, a pinned track, one in a library that takes no
// books) is left alone, and one that cannot be read is left for its next scan, which files it
// by the same rule.
func (l *Library) followAudio(ctx context.Context, op string, r bookPartRead, libs []*model.Library, rep *BookPartsReport) error {
	occ, err := l.store.FilesByEssence(ctx, r.file.Essence)
	if err != nil {
		return err
	}
	for _, o := range occ {
		if o.FilePID == r.FilePID || !o.OnDisk {
			continue
		}
		pf, err := l.store.PartFiles(ctx, []model.PID{o.FilePID})
		if waxerr.Is(err, waxerr.CodeNotFound) {
			continue // gone since the audio's files were listed
		}
		if err != nil {
			return err
		}
		f := pf[0]
		lib := libraryByID(libs, f.LibraryID)
		if partRefusal(op, f, lib, true) != nil {
			continue
		}
		if err := l.readPart(ctx, lib, f.FilePID, string(f.Path), scan.FileOptions{}, rep); err != nil {
			if ctx.Err() != nil {
				return err
			}
			l.log.Warn("book parts: a copy of the audio was not read again; its next scan files it", "path", f.DisplayPath, "err", err)
		}
	}
	return nil
}

// readPart reads one file again for a book-parts change, noting what the read folded away and
// made and the item the file backs after it.
func (l *Library) readPart(ctx context.Context, lib *model.Library, file model.PID, path string, opts scan.FileOptions, rep *BookPartsReport) error {
	res, _, err := l.scanner.ScanFileWith(ctx, lib, path, opts)
	if err != nil {
		return err
	}
	maps.Copy(rep.Absorbed, res.Absorbed)
	maps.Copy(rep.Created, res.Created)
	owner, err := l.store.WholeFileOwner(ctx, file)
	if err != nil {
		return err
	}
	rep.Files[file] = owner
	return nil
}

// libraryByID returns the library of libs with the rowid, or nil.
func libraryByID(libs []*model.Library, id int64) *model.Library {
	for _, lib := range libs {
		if lib.ID == id {
			return lib
		}
	}
	return nil
}
