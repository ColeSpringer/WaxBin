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

// TestPlaybackWritesCarryAsOf pins that the two playback payloads encode the
// recorded time exactly as the star and rating payloads do: omitted when absent, a
// quoted decimal string when present.
func TestPlaybackWritesCarryAsOf(t *testing.T) {
	const stamp int64 = 1 << 60
	v := stamp
	for _, tc := range []struct {
		name          string
		with, without any
	}{
		{"mark_played",
			PlayedParams{UserPID: "u", ItemPID: "i", AsOfNS: asOfToWire(&v)},
			PlayedParams{UserPID: "u", ItemPID: "i", AsOfNS: asOfToWire(nil)}},
		{"set_progress",
			ProgressParams{UserPID: "u", ItemPID: "i", PositionMS: 5, AsOfNS: asOfToWire(&v)},
			ProgressParams{UserPID: "u", ItemPID: "i", PositionMS: 5, AsOfNS: asOfToWire(nil)}},
	} {
		b, err := json.Marshal(tc.without)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "asOfNs") {
			t.Errorf("%s: a nil as-of must omit asOfNs, got %s", tc.name, b)
		}
		b, err = json.Marshal(tc.with)
		if err != nil {
			t.Fatal(err)
		}
		if want := `"asOfNs":"` + strconv.FormatInt(stamp, 10) + `"`; !strings.Contains(string(b), want) {
			t.Errorf("%s: as-of encoding = %s, want a quoted decimal string containing %s", tc.name, b, want)
		}
	}
}

// TestRecordSessionCarriesRecordedTimes pins the record_session payload's times: both
// travel as quoted decimal strings like asOfNs, and an omitted end (0, the start plus
// the play time) leaves the frame, so the field is absent rather than a zero the server
// would have to read as "not provided" anyway.
func TestRecordSessionCarriesRecordedTimes(t *testing.T) {
	const stamp int64 = 1 << 60
	b, err := json.Marshal(RecordSessionParams{UserPID: "u", ItemPID: "i", StartedAtNS: stamp, MsPlayed: 5})
	if err != nil {
		t.Fatal(err)
	}
	if want := `"startedAtNs":"` + strconv.FormatInt(stamp, 10) + `"`; !strings.Contains(string(b), want) {
		t.Errorf("start encoding = %s, want a quoted decimal string containing %s", b, want)
	}
	if strings.Contains(string(b), "endedAtNs") {
		t.Errorf("an omitted end must leave the frame, got %s", b)
	}
	var back RecordSessionParams
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.StartedAtNS != stamp || back.EndedAtNS != 0 || back.MsPlayed != 5 {
		t.Errorf("round trip = %+v, want start %d, no end, 5 ms", back, stamp)
	}
	b, err = json.Marshal(RecordSessionParams{UserPID: "u", ItemPID: "i", StartedAtNS: stamp, EndedAtNS: stamp + 1, MsPlayed: 5})
	if err != nil {
		t.Fatal(err)
	}
	if want := `"endedAtNs":"` + strconv.FormatInt(stamp+1, 10) + `"`; !strings.Contains(string(b), want) {
		t.Errorf("end encoding = %s, want a quoted decimal string containing %s", b, want)
	}
}
