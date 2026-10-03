package model

import "testing"

// span returns the years from first to last, one each.
func span(first, last int) []int {
	var out []int
	for y := first; y <= last; y++ {
		out = append(out, y)
	}
	return out
}

// repeat returns year n times.
func repeat(year, n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = year
	}
	return out
}

func TestAlbumYear(t *testing.T) {
	join := func(parts ...[]int) []int {
		var out []int
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		years []int
		want  int
	}{
		{"no member", nil, 0},
		{"no member carries a year", []int{0, 0}, 0},
		{"one member", []int{2015}, 2015},
		{"a stray year", join(repeat(2015, 11), []int{2016}), 2015},
		{"an even split goes to the earlier", join(repeat(2009, 6), repeat(1999, 6)), 1999},
		{"two records of one name", join(repeat(2001, 10), repeat(1994, 10)), 1994},
		{"two of three agree", []int{2016, 2015, 2015}, 2015},
		{"zeros do not vote", []int{0, 0, 0, 2015, 2015, 2016}, 2015},
		{"no two agree", []int{2015, 2016}, 2016},
		{"original years on a compilation", span(1971, 1985), 1985},
		{"a coincidence on a compilation", join(span(1971, 1983), []int{1975}), 1983},
	} {
		if got := AlbumYear(tc.years); got != tc.want {
			t.Errorf("%s: AlbumYear(%v) = %d, want %d", tc.name, tc.years, got, tc.want)
		}
	}
}
