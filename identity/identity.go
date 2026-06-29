// Package identity computes the content hash and the entity-identity keys that
// keep public ids stable across rescans, retags, and moves.
//
// Two hashes back file identity:
//
//   - content_hash covers the whole file and changes on any byte change
//     (computed here, by ContentHash).
//   - essence_hash covers only the decoder-independent audio essence (tags
//     stripped), so retagging a file leaves it stable. It is computed by the
//     meta adapter via WaxLabel's HashAudioEssence, which makes it real for
//     every format (not just MP3/FLAC); the scanner falls back to the content
//     hash when a file carries no hashable essence.
//
// The store uses essence-first change detection: if essence_hash is unchanged
// but content_hash changed, the file can be treated as a tag-only update. When
// the same essence appears at a new path, the existing file row is relinked
// while preserving its pid.
package identity

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"strings"

	"github.com/colespringer/waxbin/waxerr"
)

const hashReadBuf = 1 << 20 // 1 MiB streaming buffer

// ContentHash returns a stable, algorithm-tagged hash of the whole file.
func ContentHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", waxerr.Wrap(waxerr.CodeIO, "identity.ContentHash", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, bufio.NewReaderSize(f, hashReadBuf)); err != nil {
		return "", waxerr.Wrap(waxerr.CodeIO, "identity.ContentHash", err)
	}
	return tag(h), nil
}

// TrackKey derives the entity-identity key for a track: MusicBrainz recording id
// when known, else the essence hash. The store enforces uniqueness on
// (kind, key) so two encodings sharing an MBID, or identical essence, dedup to
// one logical item.
func TrackKey(mbid, essenceHash string) string {
	if m := strings.TrimSpace(mbid); m != "" {
		return "mbid:" + strings.ToLower(m)
	}
	return "essence:" + essenceHash
}

// tag renders a finished hash with its algorithm prefix for forward
// compatibility (the algorithm can change without ambiguity).
func tag(h interface{ Sum([]byte) []byte }) string {
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
