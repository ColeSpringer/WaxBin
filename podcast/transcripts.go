package podcast

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/colespringer/waxbin/internal/netsafe"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// This file is the transcript surface: the caller-supplied write (PutTranscript),
// the on-demand engine-side fetch (FetchTranscript), the read (Transcript), and
// the shared reduction that turns a transcript document into the searchable text
// the store indexes. Download's opportunistic fetch shares the same path, so a
// transcript body is reduced identically no matter how it arrived.

// PutTranscript stores a caller-supplied transcript for an episode. The body is
// reduced to searchable text exactly like a fetched one (SRT/VTT cue lines
// dropped, JSON segments joined), then indexed in transcript_fts. The size cap is
// MaxFeedBytes, the same bound a fetched transcript gets. A body that reduces to
// nothing is refused rather than stored as an empty row.
func (s *Service) PutTranscript(ctx context.Context, in model.PutTranscriptInput) error {
	const op = "podcast.PutTranscript"
	switch in.Format {
	case "srt", "vtt", "json", "text":
	default:
		return waxerr.New(waxerr.CodeInvalid, op, "transcript format must be srt|vtt|json|text: "+in.Format)
	}
	if int64(len(in.Body)) > s.cfg.MaxFeedBytes {
		return waxerr.New(waxerr.CodeInvalid, op, "transcript body exceeds the size cap")
	}
	body := transcriptToText([]byte(in.Body), in.Format)
	if strings.TrimSpace(body) == "" {
		return waxerr.New(waxerr.CodeInvalid, op, "transcript is empty after reduction")
	}
	in.Body = body
	return s.store.PutTranscript(ctx, in)
}

// FetchTranscript fetches and stores the transcript an episode's feed declared,
// on demand. Unlike Download's opportunistic best-effort fetch, errors propagate,
// so a caller (a client at queue-add or episode-open time) can tell "no
// transcript declared" from a fetch or store failure. The episode does not need
// to be downloaded, which covers a streamed-never-downloaded episode.
func (s *Service) FetchTranscript(ctx context.Context, episodePID model.PID) error {
	const op = "podcast.FetchTranscript"
	d, err := s.store.EpisodeByPID(ctx, episodePID)
	if err != nil {
		return err
	}
	url := strings.TrimSpace(d.Episode.TranscriptURL)
	if url == "" {
		return waxerr.New(waxerr.CodeInvalid, op, "episode declares no transcript url")
	}
	return s.fetchAndStoreTranscript(ctx, episodePID, url, d.Episode.TranscriptType)
}

// Transcript returns an episode's stored transcript (CodeNotFound when none is
// stored).
func (s *Service) Transcript(ctx context.Context, episodePID model.PID) (*model.Transcript, error) {
	return s.store.TranscriptByEpisode(ctx, episodePID)
}

// fetchAndStoreTranscript fetches the transcript at url through netsafe
// (MIME/size/SSRF guards), reduces it to searchable text, and stores it with its
// source URL as provenance. Errors return to the caller: FetchTranscript
// propagates them, the download path logs and moves on.
func (s *Service) fetchAndStoreTranscript(ctx context.Context, episodePID model.PID, url, mimeType string) error {
	resp, err := s.client.Do(ctx, netsafe.Request{URL: url, AcceptMIME: transcriptMIME, MaxBytes: s.cfg.MaxFeedBytes})
	if err != nil {
		return err
	}
	format := transcriptFormat(mimeType, url)
	body := transcriptToText(resp.Body, format)
	if strings.TrimSpace(body) == "" {
		return waxerr.New(waxerr.CodeInvalid, "podcast.FetchTranscript", "transcript is empty after reduction")
	}
	return s.store.PutTranscript(ctx, model.PutTranscriptInput{
		EpisodePID: episodePID, Format: format, Body: body, SourceURL: url,
	})
}

// fetchTranscript is Download's opportunistic wrapper: a failure is logged, not
// fatal to the download. It reports whether a transcript was stored.
func (s *Service) fetchTranscript(ctx context.Context, episodePID model.PID, url, mimeType string) bool {
	if err := s.fetchAndStoreTranscript(ctx, episodePID, url, mimeType); err != nil {
		s.log.Debug("transcript fetch failed", "url", url, "err", err)
		return false
	}
	return true
}

// transcriptFormat classifies a transcript by its declared MIME type, falling back
// to the URL extension.
func transcriptFormat(mimeType, url string) string {
	t := strings.ToLower(mimeType)
	switch {
	case strings.Contains(t, "json"):
		return "json"
	case strings.Contains(t, "srt"), strings.Contains(t, "subrip"), strings.HasSuffix(strings.ToLower(url), ".srt"):
		return "srt"
	case strings.Contains(t, "vtt"), strings.HasSuffix(strings.ToLower(url), ".vtt"):
		return "vtt"
	default:
		return "text"
	}
}

// transcriptToText reduces a transcript to searchable plain text: SRT/VTT cue
// numbers and timestamp lines are dropped, and a Podcasting 2.0 JSON transcript
// is reduced to its joined segment bodies, so the FTS index holds words, not
// timecodes or JSON syntax. Unknown formats (and a JSON body that is not the
// segments shape) are stored verbatim rather than losing data; FTS tokenization
// ignores the punctuation anyway.
func transcriptToText(data []byte, format string) string {
	switch format {
	case "srt", "vtt":
		return cueTextLines(data)
	case "json":
		return jsonTranscriptText(data)
	default:
		return string(data)
	}
}

// cueTextLines keeps an SRT/VTT document's text lines. It walks the raw bytes
// line by line (subslices, no per-line allocation) rather than splitting the
// whole document into a string slice: a transcript can run to MaxFeedBytes, and
// only the kept lines are worth copying. Not bufio.Scanner, whose default token
// cap would stop silently at the first over-long line and truncate the rest of
// the document.
func cueTextLines(data []byte) string {
	var b strings.Builder
	rest := data
	for len(rest) > 0 {
		line := rest
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i], rest[i+1:]
		} else {
			rest = nil
		}
		l := bytes.TrimSpace(line)
		if len(l) == 0 || string(l) == "WEBVTT" || isTimecodeLine(l) || isAllDigits(l) {
			continue
		}
		b.Write(l)
		b.WriteByte('\n')
	}
	return b.String()
}

// jsonTranscriptText joins the segment bodies of a Podcasting 2.0 JSON
// transcript, one line per segment. The spec shape is an object with a
// "segments" array; some publishers emit the bare array, so both are accepted,
// dispatched on the document's first byte so only one decode ever runs. A body
// that parses as neither (or whose segments carry no text) is returned verbatim
// so nothing is lost to a shape mismatch.
func jsonTranscriptText(data []byte) string {
	type segment struct {
		Body string `json:"body"`
	}
	trimmed := bytes.TrimSpace(data)
	var segs []segment
	switch {
	case len(trimmed) > 0 && trimmed[0] == '{':
		var doc struct {
			Segments []segment `json:"segments"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			return string(data)
		}
		segs = doc.Segments
	case len(trimmed) > 0 && trimmed[0] == '[':
		if err := json.Unmarshal(data, &segs); err != nil {
			return string(data)
		}
	default:
		return string(data)
	}
	var b strings.Builder
	for _, s := range segs {
		if t := strings.TrimSpace(s.Body); t != "" {
			b.WriteString(t)
			b.WriteByte('\n')
		}
	}
	if b.Len() == 0 {
		return string(data)
	}
	return b.String()
}

// isTimecodeLine reports whether a line is an SRT/VTT timestamp cue ("00:00:01,000
// --> 00:00:04,000").
func isTimecodeLine(l []byte) bool { return bytes.Contains(l, []byte("-->")) }

func isAllDigits(l []byte) bool {
	if len(l) == 0 {
		return false
	}
	for _, c := range l {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
