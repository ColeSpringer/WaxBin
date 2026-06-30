package model

// Book is the audiobook subtype of a playable_item (shares its ID). Like Track it
// carries denormalized display columns; normalized author/narrator artists and the
// series entity are resolved and linked alongside during persistence. A book is
// backed by one file (single-file M4B) or many (one part per file).
type Book struct {
	ItemID      int64
	Subtitle    string
	Author      string   // primary author display
	AuthorSort  string   // sort key for the primary author
	Authors     []string // all authors, primary first (role=author contributors)
	Narrators   []string // narrators (role=narrator contributors)
	Narrator    string   // joined narrator display
	Translators []string
	Editors     []string
	Series      string // series name (display)
	SeriesSeq   string // decimal/string sequence within the series ("1", "1.5")
	Year        int
	Publisher   string
	ASIN        string // Audible identifier
	ISBN        string
	Edition     string
	// Abridged is the abridged/unabridged flag: nil when unknown, else true for an
	// abridged edition. A pointer so "unknown" is distinct from "unabridged".
	Abridged    *bool
	Description string
	Genres      []string // resolved into item_genre links, like a track's
	Genre       string   // joined display of Genres (the denormalized column)
}

// Series groups the books of one set, the album abstraction for audiobooks. Books
// order within a series by their decimal/string sequence (Book.SeriesSeq).
type Series struct {
	ID       int64
	PID      PID
	Name     string
	SortKey  string
	MatchKey string
	MBID     string
}

// ContributorRole tags a person's credited role on an item. One role-tagged
// relation serves audiobooks (author/narrator/...) and, later, music credits.
type ContributorRole string

const (
	RoleAuthor     ContributorRole = "author"
	RoleNarrator   ContributorRole = "narrator"
	RoleTranslator ContributorRole = "translator"
	RoleEditor     ContributorRole = "editor"
)

// Contributor is one person credited on an item with a role. The person is an
// artist entity, so authors and narrators get the same dedup, sort keys, and
// browse machinery as performers.
type Contributor struct {
	ArtistPID PID
	Name      string
	Role      ContributorRole
	Position  int
}

// Chapter is a navigation point within a book. It carries two coordinate systems:
// FileStartMS/FileEndMS are offsets within FilePID's file (what is stored), and
// StartMS/EndMS are book-timeline offsets from the start of the whole book (what a
// consumer resumes against). The read path fills the book-timeline pair by
// accumulating the durations of the parts before this chapter's file; on the scan
// input path only the file-relative pair and Title are set. An end of 0 means
// "until the next chapter or the end of the file".
type Chapter struct {
	Position    int // book-timeline order, 0-based (assigned on read)
	Title       string
	StartMS     int64 // book-timeline offset (read)
	EndMS       int64 // book-timeline end offset, 0 if open-ended (read)
	FilePID     PID   // the file backing this chapter
	FileStartMS int64 // offset within FilePID's file (stored)
	FileEndMS   int64 // end offset within FilePID's file, 0 if open-ended (stored)
}

// BookDetail is the full read shape for one book: the item view plus its
// contributors, series placement, and chapter list with book-timeline offsets and
// the total (summed-across-parts) duration.
type BookDetail struct {
	Item            *ItemView
	Subtitle        string
	Authors         []string
	Narrators       []string
	Translators     []string
	Editors         []string
	Series          string
	SeriesPID       PID
	SeriesSeq       string
	Publisher       string
	ASIN            string
	ISBN            string
	Edition         string
	Abridged        *bool
	Description     string
	Contributors    []Contributor
	Chapters        []Chapter
	Files           []BookPart // backing files in reading order
	TotalDurationMS int64
}

// BookPart is one backing file of a multi-file book, in reading order.
type BookPart struct {
	FilePID     PID
	DisplayPath string
	Position    int
	DurationMS  int64
}
