package main

import (
	"testing"

	"github.com/colespringer/waxbin/model"
)

func TestDedupLosers(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		survivor model.PID
		want     []model.PID
	}{
		{"distinct", []string{"b", "c"}, "a", []model.PID{"b", "c"}},
		{"drops duplicate loser", []string{"b", "b", "c"}, "a", []model.PID{"b", "c"}},
		{"drops survivor", []string{"a", "b"}, "a", []model.PID{"b"}},
		{"all collapse", []string{"a", "a"}, "a", []model.PID{}},
		{"preserves order", []string{"c", "b", "c", "b"}, "a", []model.PID{"c", "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := dedupLosers(tc.args, tc.survivor)
			if len(got) != len(tc.want) {
				t.Fatalf("dedupLosers(%v, %q) = %v, want %v", tc.args, tc.survivor, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("dedupLosers(%v, %q) = %v, want %v", tc.args, tc.survivor, got, tc.want)
				}
			}
		})
	}
}
