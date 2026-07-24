package proxy

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// TestAsOfWireRoundTrip pins the as-of stamp's wire encoding: a nil recorded time
// encodes to 0 and is omitted from the frame (an older server drops the field and
// stamps at server-now), a real nanosecond value travels as a quoted decimal string
// (it can exceed 2^53, so a bare JSON number would not survive a JS client), and
// both decode back through AsOf to the optional the store expects.
func TestAsOfWireRoundTrip(t *testing.T) {
	// nil -> 0 -> omitted, and the omitted field decodes back to the nil (server-now) path.
	if asOfToWire(nil) != 0 {
		t.Fatalf("asOfToWire(nil) = %d, want 0", asOfToWire(nil))
	}
	b, err := json.Marshal(StarParams{UserPID: "u", ItemPID: "i", Starred: true, AsOfNS: asOfToWire(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "asOfNs") {
		t.Errorf("a nil as-of must omit asOfNs, got %s", b)
	}
	var back StarParams
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if AsOf(back.AsOfNS) != nil {
		t.Errorf("omitted as-of decoded to %v, want nil (server-now)", AsOf(back.AsOfNS))
	}

	// A real value beyond 2^53 travels as a quoted decimal string and round-trips.
	const stamp int64 = 1 << 60
	v := stamp
	b, err = json.Marshal(RatingParams{UserPID: "u", ItemPID: "i", AsOfNS: asOfToWire(&v)})
	if err != nil {
		t.Fatal(err)
	}
	if want := `"asOfNs":"` + strconv.FormatInt(stamp, 10) + `"`; !strings.Contains(string(b), want) {
		t.Errorf("as-of encoding = %s, want a quoted decimal string containing %s", b, want)
	}
	var rp RatingParams
	if err := json.Unmarshal(b, &rp); err != nil {
		t.Fatal(err)
	}
	if got := AsOf(rp.AsOfNS); got == nil || *got != stamp {
		t.Errorf("as-of round-trip = %v, want %d", got, stamp)
	}
}
