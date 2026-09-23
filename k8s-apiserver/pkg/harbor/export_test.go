package harbor

// SetLimits lowers the response limits for a test.
func SetLimits(pages int, bytes int64) (restore func()) {
	oldPages, oldBytes := maxPages, maxResponseBytes
	maxPages, maxResponseBytes = pages, bytes
	return func() { maxPages, maxResponseBytes = oldPages, oldBytes }
}
