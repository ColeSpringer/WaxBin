package scan

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
)

// backMatter is the place a back-matter part ("Epilogue", "Credits") takes, after any
// chapter a three-digit number can name.
const backMatter = 1000

var (
	// A bare number ("01", "7"), or one padded with a zero before the rest ("01 Intro").
	bareNumberRe = regexp.MustCompile(`^(?:(\d{1,3})|(0\d{1,2})(?:\D.*)?)$`)
	// A number set off from the rest by a separator ("1 - Intro", "1. Intro", "1_Intro").
	setOffNumberRe = regexp.MustCompile(`^(\d{1,3})\s*[-._)\]:]+\s*(?:[^\d\s].*)?$`)
	// A part word and its number ("Part 2", "Chapter 12 - X", "Chapter One", "Part IV",
	// "CD1"). "Book" is not one: "Book 2 - The Two Towers" names a book in a series.
	partWordRe = regexp.MustCompile(`(?i)^(?:part|pt|chapter|chap|ch|track|disc|disk|cd|section)\.?\s*(\d{1,3}|[a-z]+(?:-[a-z]+)?)(?:[^a-z0-9].*)?$`)
	// A section word, the whole name or set off from the rest ("Prologue", "Epilogue:
	// Aftermath"), so a title that only opens with one ("Introduction to Algorithms") is
	// not a section.
	sectionRe = regexp.MustCompile(`(?i)^(prologue|prolog|introduction|intro|preface|foreword|prelude|dedication|opening credits|interlude|epilogue|epilog|afterword|postscript|appendix|acknowledgements|acknowledgments|end credits|closing credits|credits)(?:\s*[-:._)\]\d].*)?$`)
	romanRe   = regexp.MustCompile(`^x{0,3}(?:ix|iv|v?i{0,3})$`)
	// The name organize gives a part on a disc of a multi-file book, "Title - 2-03", which
	// it writes only when every part kept the place its tags gave it.
	partDiscRe = regexp.MustCompile(`^.*\S\s+-\s+(\d{1,2})-(\d{2,3})$`)
)

var numberWords = map[string]int{
	"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7, "eight": 8, "nine": 9,
	"ten": 10, "eleven": 11, "twelve": 12, "thirteen": 13, "fourteen": 14, "fifteen": 15, "sixteen": 16,
	"seventeen": 17, "eighteen": 18, "nineteen": 19, "twenty": 20, "thirty": 30, "forty": 40, "fifty": 50,
	"sixty": 60, "seventy": 70, "eighty": 80, "ninety": 90,
}

var backMatterWords = map[string]bool{
	"epilogue": true, "epilog": true, "afterword": true, "postscript": true, "appendix": true,
	"acknowledgements": true, "acknowledgments": true, "end credits": true, "closing credits": true, "credits": true,
}

// partNumber reads a title or file name that numbers a part of a book rather than naming
// one, and returns the place it gives the part. A number that runs straight into words
// ("12 Rules for Life", "1Q84", "11-22-63") or has four digits ("1984") reads as a title.
// Front matter ("Prologue") and an interlude take 0, back matter sorts last.
func partNumber(name string) (int, bool) {
	name = strings.TrimSpace(name)
	if m := bareNumberRe.FindStringSubmatch(name); m != nil {
		n, _ := strconv.Atoi(m[1] + m[2])
		return n, true
	}
	if m := setOffNumberRe.FindStringSubmatch(name); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n, true
	}
	if m := partWordRe.FindStringSubmatch(name); m != nil {
		return wordNumber(strings.ToLower(m[1]))
	}
	if m := sectionRe.FindStringSubmatch(name); m != nil {
		if backMatterWords[strings.ToLower(m[1])] {
			return backMatter, true
		}
		return 0, true
	}
	return 0, false
}

// wordNumber reads the number after a part word: digits, a number word up to ninety-nine
// ("one", "twenty-one") or a roman numeral up to XXXIX.
func wordNumber(w string) (int, bool) {
	if n, err := strconv.Atoi(w); err == nil {
		return n, true
	}
	if tens, unit, ok := strings.Cut(w, "-"); ok {
		t, u := numberWords[tens], numberWords[unit]
		if t < 20 || t%10 != 0 || u == 0 || u > 9 {
			return 0, false
		}
		return t + u, true
	}
	if n, ok := numberWords[w]; ok {
		return n, true
	}
	if !romanRe.MatchString(w) {
		return 0, false
	}
	n := 0
	for i := range len(w) {
		v := romanValue[w[i]]
		if i+1 < len(w) && v < romanValue[w[i+1]] {
			v = -v
		}
		n += v
	}
	return n, true
}

var romanValue = map[byte]int{'i': 1, 'v': 5, 'x': 10}

// PartShaped reports whether a file with no ALBUM is a numbered part of the book its
// folder names, rather than a book named by its own title: a track number past the first
// or a total of several, or a title or file name that numbers a part (partNumber). A lone
// track 1 is what a tagger writes on a single-file book too.
func PartShaped(tags *model.Tags, path string) bool {
	if tags.TrackNo > 1 || tags.TrackTotal > 1 {
		return true
	}
	_, ok := namedPart(tags, path)
	return ok
}

// namedPart is the place a part's title, else its file name, gives it.
func namedPart(tags *model.Tags, path string) (int, bool) {
	if n, ok := partNumber(tags.Title); ok {
		return n, true
	}
	base := filepath.Base(path)
	return partNumber(strings.TrimSuffix(base, filepath.Ext(base)))
}

// partPosition is a book part's place on its disc: its track number, else the place its
// title or file name gives it, so an untagged "02.mp3" reads after a part tagged track 1,
// else in a managed library the place a "Title - D-NN" name organize gave it carries. The
// second result says the file states one.
func partPosition(tags *model.Tags, path string, managed bool) (int, bool) {
	if tags.TrackNo > 0 {
		return tags.TrackNo, true
	}
	if n, ok := namedPart(tags, path); ok {
		return n, true
	}
	if _, n, ok := discPartName(path); ok && managed {
		return n, true
	}
	return 0, false
}

// partDisc is a book part's disc: the tags', else a disc folder below root, else in a
// managed library the disc of a "Title - D-NN" name organize gave it, and whether the file
// states one. A name is read only in a managed library, where WaxBin chose it.
func partDisc(tags *model.Tags, root, path string, managed bool) (int, bool) {
	if tags.DiscNo > 0 {
		return tags.DiscNo, true
	}
	if d, ok := identity.FolderDisc(root, path); ok && d > 0 {
		return d, true
	}
	if d, _, ok := discPartName(path); ok && managed {
		return d, true
	}
	return 0, false
}

// discPartName reads the disc and place of a "Title - D-NN" part name.
func discPartName(path string) (disc, n int, ok bool) {
	base := filepath.Base(path)
	m := partDiscRe.FindStringSubmatch(strings.TrimSuffix(base, filepath.Ext(base)))
	if m == nil {
		return 0, 0, false
	}
	disc, _ = strconv.Atoi(m[1])
	n, _ = strconv.Atoi(m[2])
	return disc, n, disc > 0 && n > 0
}

// PartPosition is a staged book part's position in its book as its tags, its disc folder
// and its name state it, and whether they state its place on its disc.
func PartPosition(tags *model.Tags, root, path string) (int, bool) {
	disc, _ := partDisc(tags, root, path, false)
	place, stated := partPosition(tags, path, false)
	return model.PartPosition(disc, place), stated
}
