package podcast

import "time"

// SetClock replaces the service's clock for the external test package, so a test can
// pin the fetch time a sync records rather than compare it against a wall clock that
// may not have moved.
func SetClock(s *Service, now func() time.Time) { s.now = now }
