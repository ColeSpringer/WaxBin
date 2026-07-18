package main

import "testing"

// TestTagCommandRouting confirms the parent-with-RunE plus `keys` subcommand shape
// routes unambiguously: `tag keys` reaches the read subcommand while `tag <ulid>`
// reaches the parent set/list RunE. This is only sound because an item pid is a ULID
// and can never be the literal "keys", so no cobra Args/TraverseChildren tweak is needed.
func TestTagCommandRouting(t *testing.T) {
	tagCmd := newTagCmd(&globals{})

	c, _, err := tagCmd.Find([]string{"keys"})
	if err != nil {
		t.Fatalf("find `tag keys`: %v", err)
	}
	if c.Name() != "keys" {
		t.Fatalf("`tag keys` routed to %q, want the keys subcommand", c.Name())
	}

	c, rest, err := tagCmd.Find([]string{"01HZZZZZZZZZZZZZZZZZZZZZZZZ"})
	if err != nil {
		t.Fatalf("find `tag <ulid>`: %v", err)
	}
	if c.Name() != "tag" {
		t.Fatalf("`tag <ulid>` routed to %q, want the tag command", c.Name())
	}
	if len(rest) != 1 || rest[0] != "01HZZZZZZZZZZZZZZZZZZZZZZZZ" {
		t.Fatalf("`tag <ulid>` did not preserve the pid arg: %v", rest)
	}
}
