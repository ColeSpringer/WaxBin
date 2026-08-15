package meta

import (
	"context"
	"testing"
	"time"

	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/waxerr"
)

// TestFragmentedMP4ReadableButUnwritable pins the WaxLabel v1.4 contract for
// fragmented MP4: the file scans with its initial movie box read (previously it
// was rejected as an unsupported format and cataloged with a filename title),
// and a tag write is refused as a capability the file lacks, not a bad request.
func TestFragmentedMP4ReadableButUnwritable(t *testing.T) {
	ctx := context.Background()
	rate := 44100
	sig := testaudio.ReferenceSignal(rate, time.Second/2)
	p := writeTemp(t, "frag.m4a", testaudio.EncodeAs(t, "aac", "fragmented", rate, sig))

	fm, err := NewReader().Read(ctx, p)
	if err != nil {
		t.Fatalf("Read fragmented m4a: %v", err)
	}
	if len(fm.Diagnostics) != 0 {
		t.Errorf("diagnostics = %v, want none (fragmented MP4 parses now)", fm.Diagnostics)
	}
	if fm.Tags.Title != "frag" {
		t.Errorf("title = %q, want the filename fallback %q (the fixture carries no tags)", fm.Tags.Title, "frag")
	}

	_, err = NewWriter().Apply(ctx, p, []TagEdit{{Key: "TITLE", Values: []string{"Edited"}}})
	if !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Fatalf("Apply on fragmented m4a = %v (code %s), want CodeUnsupported", err, waxerr.CodeOf(err))
	}

	// The picture path refuses up front through the per-file capability gate.
	_, err = NewWriter().ApplyPicture(ctx, p, PictureEdit{Data: []byte("not-an-image")})
	if !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Fatalf("ApplyPicture on fragmented m4a = %v (code %s), want CodeUnsupported", err, waxerr.CodeOf(err))
	}
}
