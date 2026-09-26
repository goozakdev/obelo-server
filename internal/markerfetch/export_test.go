package markerfetch

import "time"

// SetProviderTimeout shortens the per-provider ceiling for a test.
func (s *Service) SetProviderTimeout(d time.Duration) { s.providerTimeout = d }
