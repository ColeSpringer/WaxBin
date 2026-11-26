package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/internal/testsock"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// bookPartFixture writes a narrated two-part book, Tome, and a stray file tagged as a track
// of another album under root, and returns the book's pid and the stray's file pid.
func bookPartFixture(t *testing.T, lib *waxbin.Library, root string) (model.PID, model.PID) {
	t.Helper()
	ctx := context.Background()
	for i := 1; i <= 2; i++ {
		writeTestFile(t, filepath.Join(root, "Author", "Tome", "0"+string(rune('0'+i))+".mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
			Title: "Chapter", Artist: "Author", AlbumArtist: "Author", Album: "Tome", Track: i,
			TXXX: []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}, Audio: testaudio.AudioWithSeed(byte(i))}))
	}
	writeTestFile(t, filepath.Join(root, "Other", "stray.mp3"), testaudio.BuildMP3WithAudio("Stray", "Someone", "Wrong", 9, testaudio.AudioWithSeed(30)))
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	pidOf := func(title string) model.PID {
		items, err := lib.Query(ctx, query.New(query.EntityItems).Where("title", query.OpIs, title).Build(), "")
		if err != nil || len(items) != 1 {
			t.Fatalf("items titled %s = %d (err %v)", title, len(items), err)
		}
		return items[0].PID
	}
	files, err := lib.ItemFiles(ctx, pidOf("Stray"))
	if err != nil || len(files) != 1 {
		t.Fatalf("stray files = %+v (err %v)", files, err)
	}
	return pidOf("Tome"), files[0].FilePID
}

// bookPartsOut decodes `book part --json`.
func bookPartsOut(t *testing.T, out string) map[string]string {
	t.Helper()
	var env struct {
		Data struct {
			Files map[string]string `json:"files"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("book part printed %q: %v", out, err)
	}
	return env.Data.Files
}

// TestBookPartFilesMarksAndWithdraws: `book part` files a file into a book at the number
// given, `book` marks the part as assigned, and --clear withdraws it.
func TestBookPartFilesMarksAndWithdraws(t *testing.T) {
	t.Parallel()
	db, root := filepath.Join(t.TempDir(), "catalog.db"), t.TempDir()
	lib := openCLILib(t, context.Background(), db, root, false)
	book, file := bookPartFixture(t, lib, root)
	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := runCLIJSON(t, db, root, "book", "part", string(file), "--book", string(book), "--number", "3")
	if err != nil {
		t.Fatalf("book part: %v", err)
	}
	if got := bookPartsOut(t, out)[string(file)]; got != string(book) {
		t.Fatalf("book part filed the file into %q, want %s", got, book)
	}
	out, err = runCLIJSON(t, db, root, "book", string(book))
	if err != nil {
		t.Fatal(err)
	}
	var shown struct {
		Data struct {
			Parts []struct {
				FilePID  string `json:"filePid"`
				Position int    `json:"position"`
				Assigned bool   `json:"assigned"`
			} `json:"parts"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &shown); err != nil || len(shown.Data.Parts) != 3 {
		t.Fatalf("book printed %s (err %v), want three parts", out, err)
	}
	if p := shown.Data.Parts[2]; p.FilePID != string(file) || p.Position != 3 || !p.Assigned {
		t.Errorf("third part = %+v, want the stray assigned at 3", p)
	}
	text, err := runCLIText(t, db, root, "book", "part", string(file), "--clear")
	if err != nil || !strings.HasPrefix(text, "Withdrew 1 part") {
		t.Fatalf("book part --clear = %q (err %v), want it to say it withdrew the part", text, err)
	}
	out, err = runCLIJSON(t, db, root, "book", string(book))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, string(file)) {
		t.Errorf("book after the withdrawal = %s, want the stray gone from it", out)
	}
}

// TestBookPartRefusesAMalformedLine: a book part with neither --book nor --clear, with both,
// or with --disc but no --number is a usage error.
func TestBookPartRefusesAMalformedLine(t *testing.T) {
	t.Parallel()
	db, root := filepath.Join(t.TempDir(), "catalog.db"), t.TempDir()
	for _, args := range [][]string{
		{"book", "part", "F"},
		{"book", "part", "F", "--book", "B", "--clear"},
		{"book", "part", "F", "--book", "B", "--disc", "2"},
		{"book", "part", "F", "--clear", "--number", "1"},
		{"book", "part", "F", "--book", "B", "--number", "-2"},
		{"book", "part", "F", "--book", "B", "--disc", "-1", "--number", "1"},
	} {
		if _, err := runCLIText(t, db, root, args...); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("%v = %v, want a usage error", args, err)
		}
	}
}

// TestBookPartThroughAServer: with a server running, book part files the parts as a job in
// the server and prints the report the job recorded.
func TestBookPartThroughAServer(t *testing.T) {
	t.Parallel()
	db, root := filepath.Join(t.TempDir(), "catalog.db"), t.TempDir()
	lib := serveCatalog(t, waxbin.Options{DBPath: db, IPCSocket: testsock.Path(t),
		Roots: []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}}})
	book, file := bookPartFixture(t, lib, root)
	out, err := runCLIJSON(t, db, root, "book", "part", string(file), "--book", string(book), "--number", "3")
	if err != nil {
		t.Fatalf("book part: %v", err)
	}
	if got := bookPartsOut(t, out)[string(file)]; got != string(book) {
		t.Fatalf("book part through the server filed the file into %q, want %s", got, book)
	}
	if list, err := lib.Jobs(context.Background(), 1); err != nil || list[0].Kind != "book-parts" {
		t.Errorf("newest job = %+v (err %v), want the server's book-parts job", list, err)
	}
}

// TestImportFilesAnAcquiredPartIntoItsBook: import --book files an untagged file as the
// named book's part at --part, the plan and the report marking it assigned, and --book-pid
// adds a later part to the book it names.
func TestImportFilesAnAcquiredPartIntoItsBook(t *testing.T) {
	t.Parallel()
	db, root := filepath.Join(t.TempDir(), "catalog.db"), t.TempDir()
	openCLILib(t, context.Background(), db, root, false).Close()
	src := filepath.Join(t.TempDir(), "upload.mp3")
	writeTestFile(t, src, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "Two", Audio: testaudio.AudioWithSeed(2)}))
	args := []string{"import", src, "--as", "book", "--book", "Dune", "--author", "Frank Herbert", "--part", "2", "--parts", "3"}
	out, err := runCLIJSON(t, db, root, args...)
	if err != nil {
		t.Fatalf("import plan: %v", err)
	}
	var plan struct {
		Data struct {
			Actions []struct {
				Assigned bool `json:"assigned"`
			} `json:"actions"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &plan); err != nil || len(plan.Data.Actions) != 1 || !plan.Data.Actions[0].Assigned {
		t.Fatalf("plan = %s (err %v), want the file assigned", out, err)
	}
	out, err = runCLIJSON(t, db, root, append(args, "--apply")...)
	if err != nil {
		t.Fatalf("import --apply: %v", err)
	}
	var rep struct {
		Data struct {
			Files []struct {
				ItemPID  string `json:"itemPid"`
				Assigned bool   `json:"assigned"`
			} `json:"files"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil || len(rep.Data.Files) != 1 || !rep.Data.Files[0].Assigned {
		t.Fatalf("report = %s (err %v), want the file assigned", out, err)
	}
	book := rep.Data.Files[0].ItemPID
	later := filepath.Join(t.TempDir(), "later.mp3")
	writeTestFile(t, later, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "Three", Audio: testaudio.AudioWithSeed(3)}))
	if _, err := runCLIJSON(t, db, root, "import", later, "--as", "book", "--book-pid", book, "--part", "3", "--apply"); err != nil {
		t.Fatalf("import --book-pid: %v", err)
	}
	out, err = runCLIJSON(t, db, root, "book", book)
	if err != nil {
		t.Fatal(err)
	}
	var shown struct {
		Data struct {
			Title string `json:"title"`
			Parts []struct {
				Position int `json:"position"`
			} `json:"parts"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &shown); err != nil || shown.Data.Title != "Dune" || len(shown.Data.Parts) != 2 ||
		shown.Data.Parts[0].Position != 2 || shown.Data.Parts[1].Position != 3 {
		t.Errorf("book = %s (err %v), want Dune with parts 2 and 3", out, err)
	}
	if _, err := runCLIText(t, db, root, "import", later, "--as", "book", "--author", "Someone"); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("--author with no book = %v, want a usage error", err)
	}
}

// TestBookPartClearCountsWhatItWithdrew: --clear over a file with an assignment and one with
// none says it withdrew the one.
func TestBookPartClearCountsWhatItWithdrew(t *testing.T) {
	t.Parallel()
	db, root := filepath.Join(t.TempDir(), "catalog.db"), t.TempDir()
	lib := openCLILib(t, context.Background(), db, root, false)
	book, file := bookPartFixture(t, lib, root)
	parts, err := lib.ItemFiles(context.Background(), book)
	if err != nil {
		t.Fatal(err)
	}
	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLIJSON(t, db, root, "book", "part", string(file), "--book", string(book), "--number", "3"); err != nil {
		t.Fatal(err)
	}
	text, err := runCLIText(t, db, root, "book", "part", string(file), string(parts[0].FilePID), "--clear")
	if err != nil || !strings.HasPrefix(text, "Withdrew 1 part\n") {
		t.Errorf("book part --clear = %q (err %v), want one part withdrawn", text, err)
	}
}

// TestImportRefusesABookPIDBesideATitle: --book-pid names the book, so --book or --author
// beside it is a usage error rather than ignored.
func TestImportRefusesABookPIDBesideATitle(t *testing.T) {
	t.Parallel()
	db, root := filepath.Join(t.TempDir(), "catalog.db"), t.TempDir()
	lib := openCLILib(t, context.Background(), db, root, false)
	book, _ := bookPartFixture(t, lib, root)
	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "upload.mp3")
	writeTestFile(t, src, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "Two", Audio: testaudio.AudioWithSeed(2)}))
	for _, extra := range [][]string{{"--book", "Dune"}, {"--author", "Frank Herbert"}} {
		args := append([]string{"import", src, "--as", "book", "--book-pid", string(book)}, extra...)
		if _, err := runCLIText(t, db, root, args...); !waxerr.Is(err, waxerr.CodeInvalid) || !strings.Contains(err.Error(), "--book-pid") {
			t.Errorf("%v = %v, want the flags refused together", args, err)
		}
	}
}
