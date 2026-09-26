package lyricfetch

import "time"

// SetAskTimeout shortens how long one provider is given, so a test of the limit
// need not wait out the real one.
func SetAskTimeout(s *Service, d time.Duration) { s.askTimeout = d }
