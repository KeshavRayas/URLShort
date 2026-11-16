package link

import (
	"time"

	"github.com/google/uuid"
)

// Link is one short code and its target. Click counts live on the row so the
// management API can show them without scanning events.
type Link struct {
	Code      string
	TargetURL string
	UserID    uuid.UUID
	CreatedAt time.Time
	ExpiresAt *time.Time
	DeletedAt *time.Time
	Clicks    int64
}

// Expired reports whether the link's own expiry has passed. A nil ExpiresAt
// means the link lives until deleted.
func (l Link) Expired(now time.Time) bool {
	return l.ExpiresAt != nil && !now.Before(*l.ExpiresAt)
}

// Cached is the Redis representation. Field names are short because this value
// is read on every redirect.
type Cached struct {
	TargetURL string     `json:"u"`
	ExpiresAt *time.Time `json:"e,omitempty"`
}
