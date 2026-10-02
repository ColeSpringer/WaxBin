package scan

import (
	"context"
	"os"
	"strings"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
)

// The folder rule: a book's parts share a folder (identity.AlbumFolder, so a disc
// subfolder counts as its book's), and a file whose tags name no book of its own joins the
// book its folder holds. That is a book with no ALBUM, whatever made it a book, or a track
// the rule made one (no force or lock) outside a library declared music and with no cue
// sheet beside it to make it a rip. It joins the book its ALBUM names, or with no ALBUM
// the folder's only book when it is a numbered part (PartShaped); a track joins only a
// strong book, one the library's media, an .m4b, an audiobook media type or a narrator
// credit made, or one already of several parts. A genre alone names what one file holds,
// so it never pulls its folder in.
//
// A file with an ALBUM joins the book it names: one this walk read in the folder, then the
// book it already backs, then one the catalog holds there. A numbered part with no ALBUM
// joins the folder's only book, counting the books this walk read there and those the
// catalog holds but not one the part alone makes up. Failing those a file stays in the
// book it already backs while its tags name no other (a moved part, a retagged one, a book
// named for a folder since renamed): a book with no ALBUM in any book, a track in one of
// several parts. A part that took the folder's only book before the walk read a second one
// leaves it again when the folder settles, so the order of the walk decides nothing. A
// walk cut short before it settles a folder leaves a part it had not joined for a forced
// scan.

// folderBook is a book a folder holds, strong when it can take in a file made a track.
// probe is a file of a one-part catalog book whose tags say whether it is strong, read
// on first need.
type folderBook struct {
	model.FolderBook
	strong bool
	probe  string
}

// joinedBy says how the folder rule found a file its book.
type joinedBy int

const (
	byName joinedBy = iota + 1 // the book its ALBUM names
	byOnly                     // the folder's only book, for a numbered part with no ALBUM
	byStay                     // the book the file already backs
)

// looseFile is a file the walk read in a folder whose tags named no book of its own: the
// item it backs after its put, whether the folder's only book took it as the walk passed,
// and the counter its outcome took.
type looseFile struct {
	path              string
	album             string
	track, part, only bool
	item              model.PID
	counted           *int
}

// unreadFile is a file the walk passed without reading, unchanged since the last scan, or
// one of the folder's outside a sub-path walk.
type unreadFile struct {
	path    string
	known   model.ScopedFile
	outside bool
}

// folderState is what a walk knows of one album folder: the books its reads made there,
// the files it read that named no book, the files it passed unread, and the folder's books
// in the catalog, loaded on first need.
type folderState struct {
	folder  string
	books   []folderBook
	loose   []looseFile
	unread  []unreadFile
	catalog []folderBook
	loaded  bool
}

// bookFolder is the folder rule's album folder for path, empty for a file straight under
// the root.
func bookFolder(root, path string) string {
	f := identity.AlbumFolder(root, path)
	if pathx.SamePath(f, root) || !pathx.UnderRoot(root, f) {
		return ""
	}
	return f
}

// HasCueSheet reports a .cue beside the file, the sheet scanCueSidecar reads.
func HasCueSheet(path string) bool {
	_, err := os.Stat(sidecarPath(path, ".cue"))
	return err == nil
}

// joins reports whether the folder rule may give a file a book, and whether doing so
// makes a track a book (see the rule above). rip says a cue sheet lies beside the file.
func joins(kind model.Kind, ruled bool, album string, lib *model.Library, rip bool) (ok, track bool) {
	if kind == model.KindBook {
		return album == "", false
	}
	return ruled && lib.MediaType() != model.MediaMusic && !rip, true
}

// named reports whether an ALBUM names a book: its title, or the title its key was made
// from (a catalog-only title edit leaves the key as it was). An empty ALBUM names any.
func named(album string, b model.FolderBook) bool {
	if album == "" || NamesBook(album, b.Title) {
		return true
	}
	t := identity.MatchKey(cleanBookTitle(album))
	if rest, ok := strings.CutPrefix(b.Key, "book:"); ok {
		if seg := strings.Split(rest, "\x1f"); len(seg) == 3 && seg[1] == t {
			return true
		}
	}
	return false
}

// NamesBook reports whether an ALBUM names a book of the title.
func NamesBook(album, title string) bool {
	return identity.MatchKey(cleanBookTitle(album)) == identity.MatchKey(title)
}

// names reports whether an ALBUM names a book (named), or, for a book keyed by an
// identifier, which keeps no title for a catalog-only title edit to leave behind, whether
// it names the album the book's primary file carries, read once per walk.
func (s *Scanner) names(ctx context.Context, sc *scanCtx, album string, b *model.FolderBook) bool {
	if named(album, *b) {
		return true
	}
	if strings.HasPrefix(b.Key, "book:") || len(b.Primary) == 0 {
		return false
	}
	primary := string(b.Primary)
	tagged, ok := sc.albums[primary]
	if !ok {
		if fm, err := s.inspect(ctx, primary); err == nil {
			tagged = strings.TrimSpace(fm.Tags.Album)
		}
		if sc.albums == nil {
			sc.albums = map[string]string{}
		}
		sc.albums[primary] = tagged
	}
	return tagged != "" && identity.MatchKey(cleanBookTitle(album)) == identity.MatchKey(cleanBookTitle(tagged))
}

// namedBook returns the book among a folder's that an ALBUM names, or nil.
func (s *Scanner) namedBook(ctx context.Context, sc *scanCtx, books []folderBook, album string) *folderBook {
	for i := range books {
		if s.names(ctx, sc, album, &books[i].FolderBook) {
			return &books[i]
		}
	}
	return nil
}

// onlyBook returns the one book a folder holds for a numbered part with no ALBUM: the
// books this walk read there and those the catalog holds, less own, a book the part alone
// makes up. It is nil when there are none or several; walked says it is one this walk read.
func (s *Scanner) onlyBook(ctx context.Context, lib *model.Library, root string, f *folderState, folder string, own model.PID) (*folderBook, bool, error) {
	catalog, err := s.folderCatalog(ctx, lib, root, folder, f)
	if err != nil {
		return nil, false, err
	}
	var read []folderBook
	if f != nil {
		read = f.books
	}
	var only *folderBook
	walked := false
	seen := map[model.PID]bool{own: true}
	for _, set := range []struct {
		books  []folderBook
		walked bool
	}{{read, true}, {catalog, false}} {
		for i := range set.books {
			b := &set.books[i]
			if seen[b.ItemPID] {
				continue
			}
			seen[b.ItemPID] = true
			if only != nil {
				return nil, false, nil
			}
			only, walked = b, set.walked
		}
	}
	return only, walked, nil
}

// ownBook is the book a file alone makes up, by its standing, or empty.
func ownBook(st *model.FileStanding) model.PID {
	if st != nil && st.Book != nil && st.Book.Files == 1 && st.Role != "alternate" {
		return st.Book.ItemPID
	}
	return ""
}

// stays reports a file the stay rule keeps in the book it backs: a book in any book, a
// track only in one of several parts.
func stays(st *model.FileStanding, track bool) bool {
	return st != nil && st.Book != nil && (!track || st.Book.Files > 1)
}

// folderState returns the walk's state for folder, created and opened when create is set,
// or nil outside a walk.
func (sc *scanCtx) folderState(folder string, create bool) *folderState {
	if sc.folders == nil || folder == "" {
		return nil
	}
	f := sc.folders[folder]
	if f == nil && create {
		f = &folderState{folder: folder}
		sc.folders[folder] = f
		sc.open = append(sc.open, folder)
	}
	return f
}

// addBook records a book a read made in the folder, once per item; a second read of one
// can only strengthen it.
func (f *folderState) addBook(b folderBook) {
	for i := range f.books {
		if f.books[i].ItemPID == b.ItemPID {
			f.books[i].strong = f.books[i].strong || b.strong
			return
		}
	}
	f.books = append(f.books, b)
}

// adoption returns the book the folder rule has a file join, or nil, for a file joins
// allows (see the rule above), with how it found it. walked says the book is one this walk
// read, which a book read later in the folder cannot outrank.
func (s *Scanner) adoption(ctx context.Context, lib *model.Library, root, folder string, sc *scanCtx, track, part bool, album string, standing *model.FileStanding) (*model.FolderBook, bool, joinedBy, error) {
	f := sc.folderState(folder, true)
	if album != "" {
		if f != nil {
			if b := s.namedBook(ctx, sc, f.books, album); b != nil && (!track || b.strong) {
				return &b.FolderBook, true, byName, nil
			}
		}
		if stays(standing, track) && s.names(ctx, sc, album, standing.Book) {
			return standing.Book, false, byStay, nil
		}
		if folder == "" {
			return nil, false, 0, nil
		}
		catalog, err := s.folderCatalog(ctx, lib, root, folder, f)
		if err != nil {
			return nil, false, 0, err
		}
		if b := s.namedBook(ctx, sc, catalog, album); b != nil && (!track || s.strong(ctx, b)) {
			return &b.FolderBook, false, byName, nil
		}
		return nil, false, 0, nil
	}
	if part && folder != "" {
		b, walked, err := s.onlyBook(ctx, lib, root, f, folder, ownBook(standing))
		if err != nil {
			return nil, false, 0, err
		}
		if b != nil && (!track || s.strong(ctx, b)) {
			return &b.FolderBook, walked, byOnly, nil
		}
	}
	if stays(standing, track) {
		return standing.Book, false, byStay, nil
	}
	return nil, false, 0, nil
}

// folderCatalog returns the books the catalog holds in a folder, cached on the folder's
// walk state when there is one. A book of several parts, or any book in a library declared
// audiobook, is strong; a one-part book's strength waits on its file's tags.
func (s *Scanner) folderCatalog(ctx context.Context, lib *model.Library, root, folder string, f *folderState) ([]folderBook, error) {
	if f != nil && f.loaded {
		return f.catalog, nil
	}
	standing, err := s.cat.FolderStanding(ctx, lib.ID, root, folder)
	if err != nil {
		return nil, err
	}
	var books []folderBook
	seen := map[model.PID]bool{}
	for _, st := range standing {
		if st.Book == nil || seen[st.ItemPID] {
			continue
		}
		seen[st.ItemPID] = true
		b := folderBook{FolderBook: *st.Book, strong: st.Book.Files > 1 || lib.MediaType() == model.MediaAudiobook}
		if !b.strong {
			b.probe = string(st.Path)
		}
		books = append(books, b)
	}
	if f != nil {
		f.catalog, f.loaded = books, true
	}
	return books, nil
}

// strong reports whether a catalog book can take in a file made a track, reading the tags
// of a one-part book's file the first time it is asked.
func (s *Scanner) strong(ctx context.Context, b *folderBook) bool {
	if b.probe != "" {
		if fm, err := s.inspect(ctx, b.probe); err == nil && fm.Tags.BookSignal == model.BookTagSignal {
			b.strong = true
		}
		b.probe = ""
	}
	return b.strong
}

// inspect reads a file's tags without hashing its audio when the reader can.
func (s *Scanner) inspect(ctx context.Context, path string) (*meta.FileMeta, error) {
	if in, ok := s.reader.(meta.Inspector); ok {
		return in.Inspect(ctx, path)
	}
	return s.reader.Read(ctx, path)
}

// leaveFolders settles each folder the walk has left for good, which is each open folder
// not holding path; an empty path settles them all.
func (s *Scanner) leaveFolders(ctx context.Context, lib *model.Library, root, walkRoot, path string, sc *scanCtx, res *Result) {
	for len(sc.open) > 0 {
		top := sc.open[len(sc.open)-1]
		if path != "" && pathx.UnderRoot(top, path) {
			return
		}
		if f := sc.folders[top]; f != nil {
			s.settleFolder(ctx, lib, root, walkRoot, f, sc, res)
		}
		sc.open = sc.open[:len(sc.open)-1]
		delete(sc.folders, top)
	}
}

// settleFolder applies the folder rule to the files of a folder the walk read before the
// books they join, and to those it passed unread, once a book read in the folder has given
// them one to join: the unchanged files it walked past, and when the folder reaches past
// a sub-path walk (a disc folder scanned alone) the cataloged files outside it. A part
// that took the folder's only book as the walk passed leaves it when the walk has since
// read another. Each decision counts the books the catalog holds once the walk has left
// the folder. An unread file's tags are checked before it is read in full.
func (s *Scanner) settleFolder(ctx context.Context, lib *model.Library, root, walkRoot string, f *folderState, sc *scanCtx, res *Result) {
	if len(f.books) == 0 || ctx.Err() != nil {
		return
	}
	f.loaded = false
	catalog, err := s.folderCatalog(ctx, lib, root, f.folder, f)
	if err != nil {
		s.log.Warn("folder rule: listing the folder", "folder", f.folder, "err", err)
		return
	}
	sole := map[model.PID]bool{}
	for _, b := range catalog {
		sole[b.ItemPID] = b.Files == 1
	}
	// settled is the book a file joins now, by its ALBUM or as a numbered part with none.
	settled := func(album string, part bool, item model.PID) *folderBook {
		if album != "" {
			return s.namedBook(ctx, sc, f.books, album)
		}
		if !part {
			return nil
		}
		own := model.PID("")
		if sole[item] {
			own = item
		}
		b, _, err := s.onlyBook(ctx, lib, root, f, f.folder, own)
		if err != nil {
			s.log.Warn("folder rule: listing the folder", "folder", f.folder, "err", err)
		}
		return b
	}
	for _, l := range f.loose {
		b := settled(l.album, l.part, l.item)
		switch {
		case b != nil && b.ItemPID != l.item && (!l.track || s.strong(ctx, b)):
			s.rejoin(ctx, lib, root, l.path, sc, res, l.counted, false)
		case b == nil && l.only:
			s.rejoin(ctx, lib, root, l.path, sc, res, l.counted, true)
		}
	}
	unread := f.unread
	if !pathx.UnderRoot(walkRoot, f.folder) {
		standing, err := s.cat.FolderStanding(ctx, lib.ID, root, f.folder)
		if err != nil {
			s.log.Warn("folder rule: listing the folder", "folder", f.folder, "err", err)
		}
		for _, st := range standing {
			if p := string(st.Path); !pathx.UnderRoot(walkRoot, p) {
				unread = append(unread, unreadFile{path: p, known: model.ScopedFile{ItemPID: st.ItemPID, KindLocked: st.KindLocked}, outside: true})
			}
		}
	}
	read := map[model.PID]bool{}
	for _, b := range f.books {
		read[b.ItemPID] = true
	}
	for _, u := range unread {
		if u.known.KindLocked || read[u.known.ItemPID] {
			continue
		}
		fm, err := s.inspect(ctx, u.path)
		if err != nil {
			continue
		}
		album := strings.TrimSpace(fm.Tags.Album)
		ok, track := joins(EffectiveKind(&fm.Tags, lib, "", ""), true, album, lib, len(fm.Tags.Chapters) == 0 && HasCueSheet(u.path))
		if !ok {
			continue
		}
		if b := settled(album, PartShaped(&fm.Tags, u.path), u.known.ItemPID); b != nil && b.ItemPID != u.known.ItemPID && (!track || s.strong(ctx, b)) {
			counted := &res.Unchanged
			if u.outside {
				counted = nil
			}
			s.rejoin(ctx, lib, root, u.path, sc, res, counted, false)
		}
	}
}

// rejoin reads a file again for the folder rule and moves its count from the outcome it
// took to the one the new read gave it. It writes the file only when it joins a book, or
// with undo set, when the folder rule is left out of the read: a part that took the
// folder's only book before the walk read another is then a book of its own again. A
// file the walk did not visit (counted nil) takes no count.
func (s *Scanner) rejoin(ctx context.Context, lib *model.Library, root, path string, sc *scanCtx, res *Result, counted *int, undo bool) {
	var again Result
	sc.adoptOnly, sc.unadopt = !undo, undo
	err := s.scanAudioFile(ctx, lib, root, path, &again, sc, "")
	sc.adoptOnly, sc.unadopt = false, false
	if err != nil {
		s.log.Warn("folder rule: joining a book", "path", path, "err", err)
		return
	}
	if counted == nil || again.ItemsCreated+again.ItemsUpdated+again.SidecarsUpdated+again.Copies+again.Unchanged == 0 {
		return
	}
	*counted--
	res.ItemsCreated += again.ItemsCreated
	res.ItemsUpdated += again.ItemsUpdated
	res.SidecarsUpdated += again.SidecarsUpdated
	res.Copies += again.Copies
	res.Unchanged += again.Unchanged
}
