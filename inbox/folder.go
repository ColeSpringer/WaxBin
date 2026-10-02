package inbox

import (
	"path/filepath"
	"strings"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/organize"
	"github.com/colespringer/waxbin/scan"
)

// stagedBook is a book a staging folder holds, named by a staged file's album, and the
// files that make it up.
type stagedBook struct {
	title, key string
	strong     bool
	files      int
}

// joinFolders applies the scan's folder rule (scan/folder.go) to the folders files were
// staged in, the source folder included, since an import is often handed a book's own
// folder, unless it is a configured inbox folder; the folder a file handed over alone
// sits in (a host's upload folder as often as a book's) names no book. A file whose tags
// name no book of its own joins the book its folder holds: a track whose album names one,
// or with no album the folder's only one when it is a numbered part, and only a book a
// tag of its own made (a narrator, an .m4b, an audiobook media type), one of several
// parts, or one bound for a library declared audiobook; a numbered book part with no
// album joins the folder's only book, or failing one takes the folder's name. A track
// the request forced, or one bound for its only library when that is declared music,
// joins nothing, and nor does a file a cue sheet beside it makes a rip. When files are
// routed by kind, a track that joins a book goes with it to the library books go to.
//
// A joined file lands in its book's folder after it, so its destination catalogs it into
// that book whatever author its own tags give: a mixed library's folder rule takes a track
// whose album names the book, and the book a file with no album was given. A library
// declared audiobook keys a file whose own album names the book by its own tags, so there
// a part credited to another artist is cataloged as a book of that artist's, and is
// planned as one.
func (s *Service) joinFolders(req Request, files []*staged) {
	byFolder := map[string][]*staged{}
	var folders []string
	for _, f := range files {
		if f.Library == nil || (f.Outcome != OutcomeImport && f.Outcome != OutcomeDuplicate) {
			continue
		}
		root := stagingRoot(req.Source, f.Src)
		dir := identity.AlbumFolder(root, f.Src)
		if (req.Inbox || req.Source == "") && pathx.SamePath(dir, root) {
			continue
		}
		if byFolder[dir] == nil {
			folders = append(folders, dir)
		}
		byFolder[dir] = append(byFolder[dir], f)
	}
	music := req.Route == nil && req.Library.MediaType() == model.MediaMusic
	for _, dir := range folders {
		var books []stagedBook
		for _, f := range byFolder[dir] {
			if f.Kind != model.KindBook || strings.TrimSpace(f.raw.Album) == "" || f.book == "" {
				continue
			}
			strong := f.tags.BookSignal == model.BookTagSignal || f.Library.MediaType() == model.MediaAudiobook
			if i := bookIndex(books, f.book); i >= 0 {
				books[i].strong = books[i].strong || strong
				books[i].files++
				continue
			}
			books = append(books, stagedBook{title: scan.BookTitle(f.tags), key: f.book, strong: strong, files: 1})
		}
		for i := range books {
			books[i].strong = books[i].strong || books[i].files > 1
		}
		var only *stagedBook
		if len(books) == 1 {
			only = &books[0]
		}
		for _, f := range byFolder[dir] {
			if f.Outcome != OutcomeImport {
				continue
			}
			album := strings.TrimSpace(f.raw.Album)
			part := album == "" && scan.PartShaped(&f.raw, f.Src)
			switch {
			case f.Kind == model.KindTrack:
				if f.KindForced || music || (len(f.raw.Chapters) == 0 && scan.HasCueSheet(f.Src)) {
					continue
				}
				if b := namedBook(books, album); b != nil && b.strong {
					s.makeBook(req, f, b, "")
				} else if part && only != nil && only.strong {
					s.makeBook(req, f, only, only.title)
				}
			case album == "" && part && only != nil:
				s.makeBook(req, f, only, only.title)
			case album == "" && part:
				s.makeBook(req, f, nil, filepath.Base(dir))
			}
		}
	}
}

func bookIndex(books []stagedBook, key string) int {
	for i := range books {
		if books[i].key == key {
			return i
		}
	}
	return -1
}

// namedBook returns the book among a folder's that an album names, or nil.
func namedBook(books []stagedBook, album string) *stagedBook {
	if album == "" {
		return nil
	}
	for i := range books {
		if scan.NamesBook(album, books[i].title) {
			return &books[i]
		}
	}
	return nil
}

// makeBook gives a staged file the book the folder rule found it a part of (book, nil
// for a book named for its folder): album is the book's title for a file whose tags name
// no album, which the cataloging scan is told (Action.Album), since the name the file is
// placed under no longer shows its folder. The file is routed, keyed and rendered again
// as a book, keyed by the book it joins unless its own album keys it at its destination.
func (s *Service) makeBook(req Request, f *staged, book *stagedBook, album string) {
	f.Joined = book != nil
	tags := f.raw
	if album != "" {
		tags.Album, f.Album = album, album
	}
	meta.PromoteBookFields(&tags)
	f.Kind, f.tags = model.KindBook, tags
	root := stagingRoot(req.Source, f.Src)
	f.place, f.placeStated = scan.PartPosition(&tags, root, f.Src)
	f.Position = f.place
	lib, reason := resolveLibrary(req, model.KindBook)
	if lib == nil {
		if reason == "" {
			reason = "no unambiguous managed library for kind " + string(model.KindBook)
		}
		f.Outcome, f.Reason = OutcomeQuarantine, reason
		return
	}
	f.Library = lib
	f.book = scan.BookIdentityKey(tags)
	if book != nil && (album != "" || lib.MediaType() != model.MediaAudiobook) {
		f.book = book.key
	}
	prof := req.Profile
	if req.ProfileFor != nil {
		prof = req.ProfileFor(lib)
	}
	f.prof = prof
	rel, err := organize.RenderRelPath(prof, acquiredItemView(tags, f.Src, model.KindBook))
	if err != nil {
		f.Outcome, f.Reason = OutcomeQuarantine, "no destination: "+err.Error()
		return
	}
	f.rel, f.RelDst = rel, rel
}
