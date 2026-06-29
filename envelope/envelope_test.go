package envelope_test

import (
	"testing"

	"github.com/colespringer/waxbin/envelope"
	"github.com/colespringer/waxbin/waxerr"
)

type sample struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

const kind = "waxbin.test"

func TestWrapDecodeRoundTrip(t *testing.T) {
	in := sample{Name: "hello", Count: 7}
	data, err := envelope.Wrap(kind, 1, in)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	got, version, err := envelope.Decode[sample](data, kind, 3)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if version != 1 {
		t.Errorf("version = %d, want 1", version)
	}
	if got != in {
		t.Errorf("round-trip = %+v, want %+v", got, in)
	}
}

func TestUnwrapRejectsWrongKind(t *testing.T) {
	data, _ := envelope.Wrap("other.kind", 1, sample{})
	if _, _, err := envelope.Unwrap(data, kind, 1); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("want CodeInvalid for wrong kind, got %v", err)
	}
}

func TestUnwrapRejectsFutureVersion(t *testing.T) {
	data, _ := envelope.Wrap(kind, 5, sample{})
	if _, _, err := envelope.Unwrap(data, kind, 2); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("want CodeInvalid for future version, got %v", err)
	}
}

func TestUnwrapRejectsMissingVersion(t *testing.T) {
	if _, _, err := envelope.Unwrap([]byte(`{"kind":"waxbin.test","payload":{}}`), kind, 1); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("want CodeInvalid for missing version, got %v", err)
	}
}

func TestWrapRejectsEmptyKind(t *testing.T) {
	if _, err := envelope.Wrap("", 1, sample{}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("want CodeInvalid for empty kind, got %v", err)
	}
}
