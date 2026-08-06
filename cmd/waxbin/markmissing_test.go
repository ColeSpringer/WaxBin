package main

import (
	"encoding/json"
	"testing"
)

// TestMarkMissingViewJSON pins the `mark-missing --json` payload shape. The outcome
// is the reason the command answers in JSON at all: the caller it exists for is a
// worker deciding what to do next, and files-present ("the bytes really are there,
// your failure is something else") has to be tellable from marked without parsing
// the text column.
func TestMarkMissingViewJSON(t *testing.T) {
	b, err := json.Marshal([]markMissingView{
		{PID: "i1", Outcome: "marked"},
		{PID: "i2", Outcome: "files-present"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"pid":"i1","outcome":"marked"},{"pid":"i2","outcome":"files-present"}]`
	if string(b) != want {
		t.Errorf("json = %s\nwant %s", b, want)
	}
}
