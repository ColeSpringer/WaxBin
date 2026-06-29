package meta

import (
	"bufio"
	"encoding/binary"
	"io"
	"os"

	"github.com/colespringer/waxbin/model"
)

// readWAV fills audio properties from a WAV header without decoding PCM (the
// cataloging/analysis boundary: scanning never reads samples). Duration comes
// from the data chunk size and the byte rate, so WAV files carry a real catalog
// duration even though WAV has no duration tag. That keeps the analyze pass's
// duration bucketing accurate. Best-effort: a malformed header just leaves the
// fields unset.
func readWAV(path string, t *model.Tags) error {
	f, err := os.Open(path)
	if err != nil {
		return nil // best-effort; the scanner still catalogs the file
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 4096)

	var riff [12]byte
	if _, err := io.ReadFull(r, riff[:]); err != nil {
		return nil
	}
	if string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return nil
	}

	var byteRate uint32
	for {
		var hdr [8]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return nil
		}
		id := string(hdr[0:4])
		size := int64(binary.LittleEndian.Uint32(hdr[4:8]))
		switch id {
		case "fmt ":
			if size < 16 {
				return nil
			}
			buf := make([]byte, size)
			if _, err := io.ReadFull(r, buf); err != nil {
				return nil
			}
			t.Channels = int(binary.LittleEndian.Uint16(buf[2:4]))
			t.SampleRate = int(binary.LittleEndian.Uint32(buf[4:8]))
			byteRate = binary.LittleEndian.Uint32(buf[8:12])
			t.BitDepth = int(binary.LittleEndian.Uint16(buf[14:16]))
		case "data":
			if byteRate > 0 {
				t.DurationMS = size * 1000 / int64(byteRate)
			}
			return nil // duration computed; no need to read the PCM body
		default:
			if _, err := io.CopyN(io.Discard, r, size); err != nil {
				return nil
			}
		}
		if size%2 == 1 { // word-aligned pad byte
			_, _ = io.CopyN(io.Discard, r, 1)
		}
	}
}
