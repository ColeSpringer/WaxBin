package inbox

import (
	"cmp"
	"context"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/organize"
)

// numberBooks names each staged book's files the way organize names a book's files, keyed
// as the scan will group them, and orders them to land. A book the catalog already holds
// keeps its parts and its layout: the staged parts number among them, a part whose tags and
// name give no place coming after them. Otherwise the part with the strongest book signal
// lands first, so the catalog makes it the primary file whose tags own the metadata
// organize renders the folder from, and every part renders under its layout. Files the
// folder rule joined land after the parts their own tags make, which are the book they
// join, and a copy or another encoding of a part is its alternate once cataloged, numbered
// like it and told apart.
func (s *Service) numberBooks(ctx context.Context, files []*staged, spelling map[string]string) []*staged {
	groups := map[string][]*staged{}
	var keys []string
	for _, f := range files {
		if f.Outcome != OutcomeImport || f.Kind != model.KindBook || f.book == "" {
			continue
		}
		key := strconv.FormatInt(f.Library.ID, 10) + "\x00" + f.book
		if groups[key] == nil {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], f)
	}
	landing := map[*staged][]*staged{}
	for _, key := range keys {
		first := groups[key][0]
		landing[first] = s.numberBook(ctx, key, groups[key], spelling)
	}
	out := make([]*staged, 0, len(files))
	landed := map[*staged]bool{}
	for _, f := range files {
		if landed[f] {
			continue
		}
		if order, ok := landing[f]; ok {
			for _, g := range order {
				out = append(out, g)
				landed[g] = true
			}
			continue
		}
		out = append(out, f)
	}
	return out
}

// numberBook names one staged book's files and returns them in the order they land.
func (s *Service) numberBook(ctx context.Context, key string, files []*staged, spelling map[string]string) []*staged {
	sortKey := make(map[*staged]string, len(files))
	for _, f := range files {
		sortKey[f] = model.SortKey(f.Src)
	}
	slices.SortStableFunc(files, func(x, y *staged) int {
		if c := cmp.Compare(x.place, y.place); c != 0 {
			return c
		}
		return strings.Compare(sortKey[x], sortKey[y])
	})
	var lead []*staged
	partOf := map[*staged]*staged{}
	for _, f := range files {
		i := slices.IndexFunc(lead, func(p *staged) bool {
			return p.Essence != "" && p.Essence == f.Essence || p.place == f.place && model.OtherEncoding(p.file(), f.file())
		})
		switch {
		case i < 0:
			lead = append(lead, f)
		case model.CompareQuality(f.file(), lead[i].file()) < 0:
			partOf[lead[i]], lead[i] = f, f
		default:
			partOf[f] = lead[i]
		}
	}
	partFor := func(f *staged) *staged {
		for partOf[f] != nil {
			f = partOf[f]
		}
		return f
	}
	for _, f := range files {
		f.Book, f.Alternate = key, partOf[f] != nil
	}

	number := map[*staged]organize.PartNumber{}
	used := map[string]bool{}
	var last organize.PartNumber
	var owner *staged
	layout := ""
	if view, parts := s.catalogBook(ctx, lead[0]); view != nil {
		v := *view
		spellAuthor(&v, spelling)
		layout, last = s.catalogNumbers(ctx, &v, parts, lead, number, used)
	} else {
		owner = lead[0]
		for _, f := range lead {
			if f.tags.BookSignal > owner.tags.BookSignal {
				owner = f
			}
		}
		layout, last = owner.rel, stagedNumbers(lead, owner, number)
		// The author takes the spelling the catalog keeps for it, as organize files it.
		if v := acquiredItemView(owner.tags, owner.Src, model.KindBook); spellAuthor(v, spelling) {
			if rel, err := organize.RenderRelPath(owner.prof, v); err == nil {
				layout = rel
			}
		}
	}
	for _, f := range lead {
		if f.Alternate {
			continue
		}
		if n, ok := number[f]; ok {
			f.RelDst = organize.BookPartRelPath(layout, n, last, filepath.Ext(f.Src))
		} else if f == owner {
			f.RelDst = layout
		}
		used[pathx.CollisionKey(f.RelDst)] = true
	}
	for _, f := range files {
		if !f.Alternate {
			continue
		}
		part := partFor(f)
		for n := 1; ; n++ {
			rel := organize.CopyRelPath(layout, number[part], last, n, filepath.Ext(f.Src))
			if !used[pathx.CollisionKey(rel)] && !pathExists(filepath.Join(string(f.Library.Root), rel)) {
				f.RelDst, used[pathx.CollisionKey(rel)] = rel, true
				break
			}
		}
		f.Position = part.Position
	}
	order := make([]*staged, 0, len(files))
	if owner != nil {
		order = append(order, owner)
	}
	for _, joined := range []bool{false, true} {
		for _, f := range lead {
			if f != owner && !f.Alternate && f.Joined == joined {
				order = append(order, f)
			}
		}
	}
	for _, f := range files {
		if f.Alternate {
			order = append(order, f)
		}
	}
	return order
}

// catalogBook returns the book the catalog's target library holds under a staged part's
// key, with its parts, or nil.
func (s *Service) catalogBook(ctx context.Context, f *staged) (*model.ItemView, []model.ItemFileRef) {
	view, parts, err := s.store.BookByKey(ctx, f.Library.ID, f.book)
	if err != nil {
		s.log.Warn("import: reading the book a staged part joins", "src", f.Src, "err", err)
		return nil, nil
	}
	if view == nil || len(parts) == 0 {
		return nil, nil
	}
	return view, parts
}

// catalogNumbers numbers a staged book's parts among the parts the catalog holds of it,
// in the order the catalog keeps (position, its own parts first), a staged part whose
// place its tags and name do not state taking the next place after the last. A staged
// copy of a cataloged part, or another encoding of one at its place, is that part's
// other file once scanned, so it is an alternate numbered by the part. The names the
// cataloged parts take are marked used. It returns the layout the book's files sit under
// and the last number.
func (s *Service) catalogNumbers(ctx context.Context, view *model.ItemView, parts []model.ItemFileRef, lead []*staged, number map[*staged]organize.PartNumber, used map[string]bool) (string, organize.PartNumber) {
	layout, err := organize.RenderRelPath(lead[0].prof, view)
	if err != nil {
		layout = lead[0].rel
	}
	audio := make([]*model.File, len(parts))
	for i, p := range parts {
		f, err := s.store.FileByPID(ctx, p.FilePID)
		if err != nil {
			s.log.Warn("import: reading a part of the book a staged part joins", "part", p.DisplayPath, "err", err)
			continue
		}
		audio[i] = f
	}
	twin := map[*staged]int{}
	for _, f := range lead {
		for i, a := range audio {
			if a != nil && (f.Essence != "" && f.Essence == a.EssenceHash ||
				f.placeStated && f.place == parts[i].Position && model.OtherEncoding(f.file(), *a)) {
				twin[f] = i
				break
			}
		}
	}
	type entry struct {
		pos  int
		part int // the cataloged part's index, or -1
		f    *staged
	}
	all := make([]entry, 0, len(parts)+len(lead))
	next := 0
	for i, p := range parts {
		all = append(all, entry{pos: p.Position, part: i})
		next = max(next, p.Position)
	}
	for _, f := range lead {
		if _, ok := twin[f]; !ok && f.placeStated {
			next = max(next, f.place)
		}
	}
	for _, f := range lead {
		if _, ok := twin[f]; ok {
			continue
		}
		if !f.placeStated {
			next++
			f.place, f.Position = next, next
		}
		all = append(all, entry{pos: f.place, part: -1, f: f})
	}
	slices.SortStableFunc(all, func(x, y entry) int { return cmp.Compare(x.pos, y.pos) })
	places := make([]organize.PartNumber, len(all))
	for i, e := range all {
		places[i] = organize.PartAt(e.pos)
	}
	numbers, last := organize.NumberParts(places, view.PartTotal)
	partNumber := make([]organize.PartNumber, len(parts))
	for i, e := range all {
		if e.f != nil {
			number[e.f] = numbers[i]
			continue
		}
		partNumber[e.part] = numbers[i]
		used[pathx.CollisionKey(organize.BookPartRelPath(layout, numbers[i], last, filepath.Ext(parts[e.part].DisplayPath)))] = true
	}
	for f, i := range twin {
		number[f], f.Alternate, f.place, f.Position = partNumber[i], true, parts[i].Position, parts[i].Position
	}
	return layout, last
}

// stagedNumbers numbers the parts of a staged book the catalog holds none of: a lone part
// as organize names one (LonePart), several by NumberParts, padded to the owner's tagged
// part total.
func stagedNumbers(lead []*staged, owner *staged, number map[*staged]organize.PartNumber) organize.PartNumber {
	if len(lead) == 1 {
		p := organize.PartAt(owner.place)
		last, ok := organize.LonePart(p, owner.tags.TrackTotal)
		if ok {
			number[owner] = p
		}
		return last
	}
	places := make([]organize.PartNumber, len(lead))
	for i, f := range lead {
		places[i] = organize.PartAt(f.place)
	}
	numbers, last := organize.NumberParts(places, owner.tags.TrackTotal)
	for i, f := range lead {
		number[f] = numbers[i]
	}
	return last
}

// HoldBooks keeps a book's files where they are when one of the book's own parts (one its
// tags make, not a file joined to it or a copy of a part) will not land, since the rest
// were named and laid out with it: an import planned again names them around what the
// catalog then holds. Apply runs it again over the outcomes it rechecks.
func HoldBooks(actions []Action) {
	held := map[string]string{}
	for _, a := range actions {
		if a.Book != "" && !a.Joined && !a.Alternate && a.Outcome != OutcomeImport {
			if _, ok := held[a.Book]; !ok {
				held[a.Book] = filepath.Base(a.Src)
			}
		}
	}
	for i := range actions {
		a := &actions[i]
		if part, ok := held[a.Book]; ok && a.Outcome == OutcomeImport {
			a.Outcome, a.Reason = OutcomeQuarantine, "held with its book, whose part "+part+" will not land"
			a.Dst, a.RelDst = "", ""
		}
	}
}
