package scan

import "github.com/colespringer/waxbin/model"

// EffectiveKind decides whether a file is cataloged as a track or a book: a kind the
// caller forced, then a kind lock on the item behind the file, then a library declared
// audiobook (every file in it is a book), then the file's own tags (tags.BookSignal: an
// .m4b, an audiobook media type, a narrator credit or an audiobook genre). Anything else
// is a track. A music or mixed library classifies by tags alone. The walk's folder rule
// refines a tag- or library-made kind afterwards (folder.go).
func EffectiveKind(tags *model.Tags, lib *model.Library, forced, lock model.Kind) model.Kind {
	switch {
	case forced == model.KindTrack || forced == model.KindBook:
		return forced
	case lock == model.KindTrack || lock == model.KindBook:
		return lock
	case lib != nil && lib.MediaType() == model.MediaAudiobook:
		return model.KindBook
	case tags != nil && tags.BookSignal != model.NoBookSignal:
		return model.KindBook
	}
	return model.KindTrack
}

// kindOwed reports an unchanged file the fast path must still read because its item's
// kind breaks its library's rule: a track in a library declared audiobook, not pinned by
// a kind lock. A music or mixed library classifies by tags, which only a read can check,
// so a root moved off audiobook takes a forced scan to re-derive its books.
func kindOwed(lib *model.Library, known model.ScopedFile) bool {
	return lib.MediaType() == model.MediaAudiobook && known.Kind == model.KindTrack && !known.KindLocked
}
