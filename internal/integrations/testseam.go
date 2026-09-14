package integrations

// DisableURLCheckForTest turns off the save-time SSRF check on URL fields.
//
// For TESTS ONLY, and it says so in the name so a production call site is
// obvious in review. It exists because a test that points a provider at an
// httptest server is pointing it at 127.0.0.1, which the check refuses -- it
// working, not it failing. The check is covered by ssrf_test.go in this package
// and the dial-time half by internal/netguard.
func (s *Service) DisableURLCheckForTest() { s.noURLCheck = true }
