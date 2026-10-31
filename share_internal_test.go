package waxbin

import (
	"crypto/sha256"
	"reflect"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// TestTiesListInPIDOrder: a tie reads in pid order whatever order a rung's read returned
// it in, and the read's own slice is left as it was.
func TestTiesListInPIDOrder(t *testing.T) {
	t.Parallel()
	read := []*model.ItemView{{PID: "03"}, {PID: "01"}, {PID: "02"}}
	got := byPID(read)
	if len(got) != 3 || got[0].PID != "01" || got[1].PID != "02" || got[2].PID != "03" {
		t.Errorf("tie = %v %v %v, want 01 02 03", got[0].PID, got[1].PID, got[2].PID)
	}
	if read[0].PID != "03" {
		t.Errorf("the read's slice was reordered")
	}
}

// TestRefDigestCoversEveryField: refs differing in any one field digest apart, and two
// neighboring fields cannot trade bytes, so a batch's memo never answers one ref with
// another's resolution. A field added to PortableRef fails here until refDigest reads it.
func TestRefDigestCoversEveryField(t *testing.T) {
	t.Parallel()
	seen := map[[sha256.Size]byte]string{refDigest(model.PortableRef{}): "the zero ref"}
	typ := reflect.TypeFor[model.PortableRef]()
	for i := range typ.NumField() {
		var ref model.PortableRef
		f := reflect.ValueOf(&ref).Elem().Field(i)
		switch f.Kind() {
		case reflect.String:
			f.SetString("x")
		case reflect.Int, reflect.Int64:
			f.SetInt(7)
		case reflect.Slice:
			f.SetBytes([]byte("x"))
		default:
			t.Fatalf("field %s is a %s; teach refDigest and this test", typ.Field(i).Name, f.Kind())
		}
		d := refDigest(ref)
		if prev, dup := seen[d]; dup {
			t.Errorf("setting %s digests as %s does", typ.Field(i).Name, prev)
		}
		seen[d] = typ.Field(i).Name
	}
	if refDigest(model.PortableRef{Artist: "ab"}) == refDigest(model.PortableRef{Artist: "a", Title: "b"}) {
		t.Error("an artist of ab digests as artist a with title b")
	}
}
