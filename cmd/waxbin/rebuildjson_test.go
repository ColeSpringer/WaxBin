package main

import (
	"encoding/json"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/scan"
)

// TestRebuildResultCarriesLinks: a rebuild's tally counts the links its scans did not
// follow, as the scan command's does.
func TestRebuildResultCarriesLinks(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(scanResultJSON(&waxbin.ScanResult{Total: scan.Result{Links: 2}}))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Links int `json:"links"`
	}
	if err := json.Unmarshal(b, &got); err != nil || got.Links != 2 {
		t.Errorf("rebuild json = %s (err %v), want links 2", b, err)
	}
}
