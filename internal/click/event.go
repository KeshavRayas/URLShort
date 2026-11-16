// Package click defines the redirect event consumed by the analytics worker.
package click

import "time"

// Event is one resolved redirect. It is written asynchronously so the redirect
// path never waits on a database insert.
type Event struct {
	Code        string
	Referer     string
	UserAgent   string
	VisitorHash string
	ClickedAt   time.Time
}
