package main

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// TestArtRoleViewsJSON pins the `art roles --json` payload shape. The field names are
// part of the CLI contract, and the omitempty set is what makes a lock with no
// artifact readable: it arrives as a locked role with no image rather than one
// wearing a zero size and an empty format, which is the state the command exists to
// report and the only one no other JSON read can reach for a playlist or a podcast.
// roleLocked rides beside locked so a per-role pin control can tell an auxiliary slot
// held by its own pin from one held by the front cover's.
func TestArtRoleViewsJSON(t *testing.T) {
	b, err := json.Marshal(artRoleViews([]model.ArtRoleInfo{
		{
			Role: model.ArtRoleBack, Format: "png", Width: 500, Height: 500,
			SourceHash: "h1", Attribution: model.Attribution{Source: model.SourceUser}, UpdatedAt: 7,
			Locked: true,
		},
		{Role: model.ArtRoleFront, Attribution: model.Attribution{Source: model.SourceUser},
			UpdatedAt: 9, Locked: true, RoleLocked: true},
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"role":"back","format":"png","width":500,"height":500,"sourceHash":"h1",` +
		`"source":"user","updatedAt":"7","locked":true,"roleLocked":false},` +
		`{"role":"front","source":"user","updatedAt":"9","locked":true,"roleLocked":true}]`
	if string(b) != want {
		t.Errorf("json = %s\nwant %s", b, want)
	}
}

// TestArtViewKeySet pins the `art --json` field names, which are the CLI contract. Both
// producers are checked against one list, so a field reaching one payload and not the
// other, or reaching either without a decision, fails here rather than in a consumer's
// parser. artView adding `box` was itself an unpinned change to this payload.
func TestArtViewKeySet(t *testing.T) {
	want := []string{"box", "bytes", "derived", "format", "height", "level", "provider",
		"source", "sourceHash", "sourceUrl", "thumbnail", "updatedAt", "width"}
	views := map[string]map[string]any{
		"artView":         artView(&model.ArtProvenance{Format: "png"}, false, 0),
		"artViewFromBlob": artViewFromBlob(&model.ArtBlob{Format: "png", Box: 192, Thumbnail: true}),
	}
	for name, v := range views {
		got := make([]string, 0, len(v))
		for k := range v {
			got = append(got, k)
		}
		sort.Strings(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s keys = %v, want %v", name, got, want)
		}
	}
}

// TestArtViewReportsTheRungServed pins the field a client needs to make sense of a
// picture wider than the --size it asked for. The resolve rounds the box up to a
// ladder rung, so the width alone does not say which request would land on these same
// bytes; box does. A sizeless read asked for no rung and reports none.
func TestArtViewReportsTheRungServed(t *testing.T) {
	sized := artView(&model.ArtProvenance{Format: "png", Width: 192, Height: 144}, true, 192)
	if got := sized["box"]; got != 192 {
		t.Errorf("box = %v, want 192 (the rung 187 rounds up to)", got)
	}
	sizeless := artView(&model.ArtProvenance{Format: "png", Width: 400, Height: 300}, false, 0)
	if got := sizeless["box"]; got != 0 {
		t.Errorf("box = %v, want 0 for a sizeless read", got)
	}
}

// TestParseArtLockRole pins the --role mapping on `art lock`/`art unlock`. An omitted
// flag is the front cover, whose lock is the entity's own; spelling "front" out is
// refused and points at dropping the flag, so there is one way to say it.
func TestParseArtLockRole(t *testing.T) {
	if got, err := parseArtLockRole("lock", ""); err != nil || got != model.ArtRoleFront {
		t.Errorf("empty --role = %q (err %v), want front", got, err)
	}
	for _, role := range []model.ArtRole{model.ArtRoleBack, model.ArtRoleDisc,
		model.ArtRoleBooklet, model.ArtRoleBackground} {
		if got, err := parseArtLockRole("lock", string(role)); err != nil || got != role {
			t.Errorf("--role %s = %q (err %v), want it through", role, got, err)
		}
	}
	_, err := parseArtLockRole("lock", "front")
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("--role front = %v, want CodeInvalid", err)
	}
	if !strings.Contains(err.Error(), "drop --role to lock it") {
		t.Errorf("refusal = %q, want it to point at dropping the flag", err)
	}
	if _, err := parseArtLockRole("unlock", "sleeve"); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("--role sleeve = %v, want CodeInvalid", err)
	}
}

// TestArtLockLine pins what `art lock`/`art unlock` reports. The unlock cases are the
// point: releasing an auxiliary role's pin while the entity's whole art pin stands opens
// nothing, and saying "unlocked" there leaves a user wondering why enrichment still skips
// the slot. Each such line names the command that would actually open it.
func TestArtLockLine(t *testing.T) {
	const pid = model.PID("01ABC")
	cases := []struct {
		name   string
		change model.ArtLockChange
		lock   bool
		role   model.ArtRole
		want   string
	}{
		{
			name: "lock", change: model.ArtLockChange{Changed: true, StillLocked: true},
			lock: true, role: model.ArtRoleBack,
			want: "locked release_group back art for 01ABC\n",
		},
		{
			name: "lock again", change: model.ArtLockChange{StillLocked: true},
			lock: true, role: model.ArtRoleBack,
			want: "release_group back art for 01ABC was already locked\n",
		},
		{
			name: "unlock opens the slot", change: model.ArtLockChange{Changed: true},
			role: model.ArtRoleBack,
			want: "unlocked release_group back art for 01ABC\n",
		},
		{
			name: "unlock under the whole pin", change: model.ArtLockChange{Changed: true, StillLocked: true},
			role: model.ArtRoleBack,
			want: "unlocked release_group back art for 01ABC; the slot is still locked by the entity's whole art lock\n" +
				"(waxbin art unlock 01ABC --type release_group)\n",
		},
		{
			name: "unlock a role with no pin of its own", change: model.ArtLockChange{StillLocked: true},
			role: model.ArtRoleBack,
			want: "release_group back art for 01ABC had no lock of its own; the slot is still locked by the entity's whole art lock\n" +
				"(waxbin art unlock 01ABC --type release_group)\n",
		},
		{
			name: "unlock an already-open slot", change: model.ArtLockChange{},
			role: model.ArtRoleBack,
			want: "release_group back art for 01ABC had no lock of its own\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			past := "unlocked"
			if c.lock {
				past = "locked"
			}
			got := artLockLine(c.change, c.lock, past, model.ArtReleaseGroup, c.role, pid)
			if got != c.want {
				t.Errorf("line = %q\nwant %q", got, c.want)
			}
		})
	}
}
