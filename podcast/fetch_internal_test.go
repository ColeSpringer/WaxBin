package podcast

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/source"
	"github.com/colespringer/waxbin/waxerr"
)

type refusingWriter struct{}

func (refusingWriter) Write([]byte) (int, error) { return 0, errors.New("no space left on device") }

// TestFetchToBlamesALocalWriteOnTheCatalog: a Fetch that fails because the download's
// own file refused bytes is the catalog's failure, and so is one whose provider
// swallowed the refusal and reported success.
func TestFetchToBlamesALocalWriteOnTheCatalog(t *testing.T) {
	t.Parallel()
	for _, swallow := range []bool{false, true} {
		prov := &source.Mock{Type: model.SourceYouTube,
			FetchFunc: func(_ context.Context, _ source.FetchRequest, w io.Writer) (*source.FetchResult, error) {
				w.(*destWriter).w = refusingWriter{}
				if _, err := w.Write([]byte("audio")); err != nil && !swallow {
					return nil, waxerr.Wrap(waxerr.CodeIO, "youtube", err)
				}
				return &source.FetchResult{Bytes: 5, ContentHash: "sha256:x"}, nil
			}}
		_, _, err := (&Service{}).fetchTo(context.Background(), prov,
			filepath.Join(t.TempDir(), "ep.part"), source.FetchRequest{})
		if err == nil || source.IsProviderError(err) || !waxerr.Is(err, waxerr.CodeIO) {
			t.Errorf("swallowed %v: fetchTo = %v, want an io failure that is not the provider's", swallow, err)
		}
	}
}

// TestFetchToRemakesAFolderAPruneTook: Download makes the show folder outside the podcast
// lease, so an unfetch's prune can take it before the download's file exists; the fetch
// makes it again rather than failing.
func TestFetchToRemakesAFolderAPruneTook(t *testing.T) {
	t.Parallel()
	prov := &source.Mock{Type: model.SourceYouTube, Payload: []byte("audio")}
	path := filepath.Join(t.TempDir(), "Show", "ep.part")
	n, _, err := (&Service{}).fetchTo(context.Background(), prov, path, source.FetchRequest{URL: "yt://v1"})
	if err != nil || n != 5 {
		t.Fatalf("fetchTo = %d, %v; want the five bytes written into a remade folder", n, err)
	}
}
