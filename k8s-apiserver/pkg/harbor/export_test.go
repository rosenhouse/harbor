package harbor

import "time"

// SetLimits lowers the response limits for a test.
func SetLimits(pages int, bytes int64) (restore func()) {
	oldPages, oldBytes := maxPages, maxResponseBytes
	maxPages, maxResponseBytes = pages, bytes
	return func() { maxPages, maxResponseBytes = oldPages, oldBytes }
}

// SetListRetryWait changes how long a list waits before it starts over, for a test.
func SetListRetryWait(d time.Duration) (restore func()) {
	old := listRetryWait
	listRetryWait = d
	return func() { listRetryWait = old }
}
