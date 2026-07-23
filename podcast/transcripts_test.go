package podcast

import (
	"strings"
	"testing"
)

func TestTranscriptToTextSRT(t *testing.T) {
	srt := "1\n00:00:01,000 --> 00:00:04,000\nhello world\n\n2\n00:00:04,000 --> 00:00:06,000\nsecond cue\n"
	got := transcriptToText([]byte(srt), "srt")
	if strings.Contains(got, "-->") || strings.Contains(got, "00:00") {
		t.Fatalf("timecodes survived reduction: %q", got)
	}
	if !strings.Contains(got, "hello world") || !strings.Contains(got, "second cue") {
		t.Fatalf("cue text lost: %q", got)
	}
	// The bare cue numbers are dropped too.
	for _, line := range strings.Split(got, "\n") {
		if line == "1" || line == "2" {
			t.Fatalf("cue number survived: %q", got)
		}
	}
}

func TestTranscriptToTextSRTLongLinesAndCRLF(t *testing.T) {
	// A cue line far past bufio.Scanner's 64 KiB token cap must survive intact;
	// the line walker has no such limit (the reason it is not a Scanner). CRLF
	// line endings reduce the same as bare LF.
	long := strings.Repeat("word ", 20_000) + "end"
	srt := "1\r\n00:00:01,000 --> 00:00:04,000\r\n" + long + "\r\n\r\n2\r\n00:00:04,000 --> 00:00:06,000\r\nshort\r\n"
	got := transcriptToText([]byte(srt), "srt")
	if got != long+"\nshort\n" {
		t.Fatalf("long-line reduction wrong: len=%d, tail=%q", len(got), got[max(0, len(got)-40):])
	}
}

func TestTranscriptToTextJSONSegments(t *testing.T) {
	doc := `{"version":"1.0.0","segments":[
		{"speaker":"Jane","startTime":0,"endTime":5,"body":"Hello there."},
		{"speaker":"Sam","startTime":5,"endTime":9,"body":"Hi Jane."},
		{"speaker":"Sam","startTime":9,"endTime":10,"body":"  "}]}`
	got := transcriptToText([]byte(doc), "json")
	if got != "Hello there.\nHi Jane.\n" {
		t.Fatalf("json reduction = %q", got)
	}
}

func TestTranscriptToTextJSONBareArray(t *testing.T) {
	doc := `[{"body":"First."},{"body":"Second."}]`
	if got := transcriptToText([]byte(doc), "json"); got != "First.\nSecond.\n" {
		t.Fatalf("bare-array reduction = %q", got)
	}
}

func TestTranscriptToTextJSONFallsBackVerbatim(t *testing.T) {
	// Malformed JSON, a non-segments object, and segments with no text all store
	// verbatim rather than losing the body.
	for _, doc := range []string{
		`{"segments": [`,
		`{"cues":[{"text":"x"}]}`,
		`{"segments":[{"speaker":"a"},{"speaker":"b"}]}`,
	} {
		if got := transcriptToText([]byte(doc), "json"); got != doc {
			t.Fatalf("non-segments json %q reduced to %q, want verbatim", doc, got)
		}
	}
}

func TestTranscriptToTextUnknownVerbatim(t *testing.T) {
	body := "plain text transcript"
	if got := transcriptToText([]byte(body), "text"); got != body {
		t.Fatalf("text reduction = %q", got)
	}
}
