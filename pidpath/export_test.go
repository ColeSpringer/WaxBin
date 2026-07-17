package pidpath

import "github.com/colespringer/waxbin/model"

// Cached exposes the internal cache probe to the external test package, so a test can
// assert an entry was actually cached or dropped rather than infer it from behavior a
// broken cache would also produce.
func (c *Cache) Cached(pid model.PID) (Location, bool) { return c.cached(pid) }
