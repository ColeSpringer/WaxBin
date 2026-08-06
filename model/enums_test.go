package model

import "testing"

// TestItemStatesAreValid keeps the two halves of the state vocabulary agreeing:
// every state ItemStates offers as a choice must pass Valid, or a documented
// choice would be rejected at the boundary that validates it. It cannot catch a
// fifth constant added without touching either function, which nothing in Go can;
// the pointer in the playable_item.state schema comment is what helps there.
func TestItemStatesAreValid(t *testing.T) {
	states := ItemStates()
	if len(states) == 0 {
		t.Fatal("ItemStates is empty")
	}
	for _, st := range states {
		if !st.Valid() {
			t.Errorf("ItemStates offers %q but Valid rejects it", st)
		}
	}
	for _, bad := range []ItemState{"", "present ", "Present", "gone"} {
		if bad.Valid() {
			t.Errorf("%q must not be a valid item state", bad)
		}
	}
}
