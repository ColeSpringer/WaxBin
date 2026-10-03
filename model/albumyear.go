package model

// AlbumYear is the year an album shows given its members' years, zeros ignored. A year at
// least half of them share (and more than one, where there are several) is the release's,
// a stray reissue or remaster date on a few tracks aside; an even split goes to the
// earlier year. With no such year the members carry their own original years, as on a
// compilation, and the release is no older than its newest track, so it is the latest.
// It is 0 when no member carries a year. The store's albumYearExpr reads the same rule.
func AlbumYear(years []int) int {
	counts := map[int]int{}
	dated, top := 0, 0
	for _, y := range years {
		if y <= 0 {
			continue
		}
		dated++
		counts[y]++
		top = max(top, counts[y])
	}
	best := 0
	consensus := top > 1 && 2*top >= dated
	for y, n := range counts {
		switch {
		case consensus && n == top && (best == 0 || y < best):
			best = y
		case !consensus && y > best:
			best = y
		}
	}
	return best
}
