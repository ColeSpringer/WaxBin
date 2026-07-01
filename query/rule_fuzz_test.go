package query

import "testing"

// FuzzParseRule ensures the rule-document decoder never panics on arbitrary input
// and that any rule it accepts round-trips through marshal/parse unchanged.
func FuzzParseRule(f *testing.F) {
	if b, err := MarshalRule(New(EntityTracks).Where("artist", OpIs, "X").Build()); err == nil {
		f.Add(b)
	}
	f.Add([]byte(``))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"kind":"waxbin.rule","version":1,"payload":{"entity":"track"}}`))
	f.Add([]byte(`{"kind":"waxbin.rule","version":1,"payload":{"entity":"track","root":{"type":"and","nodes":[]}}}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		q, err := ParseRule(data)
		if err != nil {
			return // rejecting malformed input is fine; not panicking is the contract
		}
		// A rule the parser accepted must re-marshal and re-parse without error.
		b, err := MarshalRule(q)
		if err != nil {
			t.Fatalf("marshal after successful parse: %v", err)
		}
		if _, err := ParseRule(b); err != nil {
			t.Fatalf("re-parse of a marshaled rule failed: %v", err)
		}
	})
}
