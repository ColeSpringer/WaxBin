package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/scan"
	"github.com/spf13/cobra"
)

// TestScanResultSaysTheSubPathWasGone: a scan of a sub-path that was not there says so,
// in both outputs, so a mistyped path does not read as an empty folder.
func TestScanResultSaysTheSubPathWasGone(t *testing.T) {
	t.Parallel()
	render := func(asJSON bool) string {
		cmd := &cobra.Command{}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		if err := renderScanResult(cmd, &globals{jsonOut: asJSON}, "J1", scan.Result{SubPathGone: true}, nil); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	if out := render(false); !strings.Contains(out, "sub-path:     not found") {
		t.Errorf("scan text does not say the sub-path was gone:\n%s", out)
	}
	var env struct {
		Data struct {
			SubPathGone bool `json:"subPathGone"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(render(true)), &env); err != nil || !env.Data.SubPathGone {
		t.Errorf("scan json = %+v (err %v), want subPathGone", env, err)
	}
}

// TestScanResultSaysTheSubPathWasBehindALink: a scan of a sub-path reached through a
// link below the root walked nothing and says so, in both outputs, so it does not read
// as an empty folder.
func TestScanResultSaysTheSubPathWasBehindALink(t *testing.T) {
	t.Parallel()
	render := func(asJSON bool) string {
		cmd := &cobra.Command{}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		if err := renderScanResult(cmd, &globals{jsonOut: asJSON}, "J1", scan.Result{SubPathLinked: true}, nil); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	if out := render(false); !strings.Contains(out, "sub-path:     behind a link") {
		t.Errorf("scan text does not say the sub-path was behind a link:\n%s", out)
	}
	var env struct {
		Data struct {
			SubPathLinked bool `json:"subPathLinked"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(render(true)), &env); err != nil || !env.Data.SubPathLinked {
		t.Errorf("scan json = %+v (err %v), want subPathLinked", env, err)
	}
}

// TestScanResultCountsLinks: links the walk met and did not follow are counted in both
// outputs, so a library of linked folders does not read as empty for no reason.
func TestScanResultCountsLinks(t *testing.T) {
	t.Parallel()
	render := func(asJSON bool) string {
		cmd := &cobra.Command{}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		if err := renderScanResult(cmd, &globals{jsonOut: asJSON}, "J1", scan.Result{Links: 3}, nil); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	if out := render(false); !strings.Contains(out, "links:        3 (not followed)") {
		t.Errorf("scan text does not count the links:\n%s", out)
	}
	var env struct {
		Data struct {
			Links int `json:"links"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(render(true)), &env); err != nil || env.Data.Links != 3 {
		t.Errorf("scan json = %+v (err %v), want links 3", env, err)
	}
}

// TestScanResultSaysTheRootWasUnreachable: a scan of a root that was not there names it
// in both outputs, so an unmounted drive does not read as an empty library, and one
// root of several is told apart from the others.
func TestScanResultSaysTheRootWasUnreachable(t *testing.T) {
	t.Parallel()
	runs := []scan.Result{{LibraryName: "/mnt/music", RootUnreachable: true, WalkErrors: 1}, {LibraryName: "/srv/books"}}
	render := func(asJSON bool) string {
		cmd := &cobra.Command{}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		if err := renderScanResult(cmd, &globals{jsonOut: asJSON}, "J1", scan.Result{RootUnreachable: true, WalkErrors: 1}, runs); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	if out := render(false); !strings.Contains(out, "/mnt/music is not reachable") || strings.Contains(out, "/srv/books") {
		t.Errorf("scan text does not name the unreachable root alone:\n%s", out)
	}
	var env struct {
		Data struct {
			RootUnreachable  bool     `json:"rootUnreachable"`
			UnreachableRoots []string `json:"unreachableRoots"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(render(true)), &env); err != nil || !env.Data.RootUnreachable ||
		len(env.Data.UnreachableRoots) != 1 || env.Data.UnreachableRoots[0] != "/mnt/music" {
		t.Errorf("scan json = %+v (err %v), want the unreachable root named", env, err)
	}
}

// TestScanResultCarriesTheFoldMap: a scan that changed items' identities says so: the
// JSON maps each, and the text counts them.
func TestScanResultCarriesTheFoldMap(t *testing.T) {
	t.Parallel()
	res := scan.Result{Converted: []model.PID{"BOOK"}, Absorbed: map[model.PID]model.PID{"T2": "BOOK"},
		Created: map[model.PID]model.PID{"T9": "OLD"}}
	render := func(asJSON bool) string {
		cmd := &cobra.Command{}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		if err := renderScanResult(cmd, &globals{jsonOut: asJSON}, "J1", res, nil); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	text := render(false)
	for _, want := range []string{"converted:    1", "absorbed:     1", "split off:    1"} {
		if !strings.Contains(text, want) {
			t.Errorf("scan text lacks %q:\n%s", want, text)
		}
	}
	var env struct {
		Data struct {
			Converted []string          `json:"converted"`
			Absorbed  map[string]string `json:"absorbed"`
			Created   map[string]string `json:"created"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(render(true)), &env); err != nil || len(env.Data.Converted) != 1 ||
		env.Data.Absorbed["T2"] != "BOOK" || env.Data.Created["T9"] != "OLD" {
		t.Errorf("scan json = %+v (err %v)", env.Data, err)
	}
}
