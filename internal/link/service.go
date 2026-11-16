package link

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// Store is the durable side. Postgres is the source of truth; every method
// here is expected to be strongly consistent.
type Store interface {
	Create(ctx context.Context, l Link) error
	GetByCode(ctx context.Context, code string) (Link, error)
	GetByCodeForUser(ctx context.Context, code string, userID uuid.UUID) (Link, error)
	ListByUser(ctx context.Context, userID uuid.UUID, limit, offset int) ([]Link, error)
	SoftDelete(ctx context.Context, code string, userID uuid.UUID) error
}

// Cache is the read-through layer in front of Store. Implementations must
// treat every error as recoverable: the service degrades to the database.
type Cache interface {
	Get(ctx context.Context, code string) (Cached, bool, error)
	Set(ctx context.Context, code string, value Cached, ttl time.Duration) error
	Delete(ctx context.Context, code string) error
	GetMiss(ctx context.Context, code string) (bool, error)
	SetMiss(ctx context.Context, code string, ttl time.Duration) error
	DeleteMiss(ctx context.Context, code string) error
}

// Options configures a Service.
type Options struct {
	Store               Store
	Cache               Cache
	BaseHost            string
	CacheTTL            time.Duration
	NegativeTTL         time.Duration
	CodeLength          int
	AllowPrivateTargets bool
	Logger              *slog.Logger
	Now                 func() time.Time
}

// Service owns link creation, resolution and lifecycle rules.
type Service struct {
	store        Store
	cache        Cache
	baseHost     string
	cacheTTL     time.Duration
	negativeTTL  time.Duration
	codeLength   int
	allowPrivate bool
	log          *slog.Logger
	now          func() time.Time
}

// NewService returns a Service. Zero values are replaced with safe defaults.
func NewService(o Options) *Service {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.CodeLength <= 0 {
		o.CodeLength = 7
	}
	return &Service{
		store:        o.Store,
		cache:        o.Cache,
		baseHost:     o.BaseHost,
		cacheTTL:     o.CacheTTL,
		negativeTTL:  o.NegativeTTL,
		codeLength:   o.CodeLength,
		allowPrivate: o.AllowPrivateTargets,
		log:          o.Logger,
		now:          o.Now,
	}
}

// CreateRequest is the validated input for a new link.
type CreateRequest struct {
	TargetURL  string
	CustomCode string
	ExpiresAt  *time.Time
}

const (
	maxCodeAttempts = 5
	maxLifetime     = 5 * 365 * 24 * time.Hour
)

// Create validates the request, allocates a code and persists the link.
//
// Generated codes are random and collision-checked by the primary key: a
// duplicate insert returns ErrCodeTaken and the loop retries. Custom aliases
// surface that error to the caller instead, because retrying would be wrong.
func (s *Service) Create(ctx context.Context, userID uuid.UUID, req CreateRequest) (Link, error) {
	target, err := NormalizeTarget(req.TargetURL, s.baseHost, s.allowPrivate)
	if err != nil {
		return Link{}, err
	}

	now := s.now().UTC()
	if req.ExpiresAt != nil {
		exp := req.ExpiresAt.UTC()
		if !exp.After(now) {
			return Link{}, invalid("expires_at", "must be in the future")
		}
		if exp.Sub(now) > maxLifetime {
			return Link{}, invalid("expires_at", "must be within 5 years")
		}
		req.ExpiresAt = &exp
	}

	base := Link{
		TargetURL: target,
		UserID:    userID,
		CreatedAt: now,
		ExpiresAt: req.ExpiresAt,
	}

	if req.CustomCode != "" {
		if !ValidCode(req.CustomCode) {
			return Link{}, invalid("custom_code", "must be 3-32 letters or digits")
		}
		if Reserved(req.CustomCode) {
			return Link{}, invalid("custom_code", "is reserved")
		}
		base.Code = req.CustomCode
		if err := s.store.Create(ctx, base); err != nil {
			if errors.Is(err, ErrCodeTaken) {
				return Link{}, ErrCodeTaken
			}
			return Link{}, fmt.Errorf("create link: %w", err)
		}
		s.warm(ctx, base)
		return base, nil
	}

	for attempt := 0; attempt < maxCodeAttempts; attempt++ {
		code, err := NewCode(s.codeLength)
		if err != nil {
			return Link{}, err
		}
		candidate := base
		candidate.Code = code
		if err := s.store.Create(ctx, candidate); err != nil {
			if errors.Is(err, ErrCodeTaken) {
				continue
			}
			return Link{}, fmt.Errorf("create link: %w", err)
		}
		s.warm(ctx, candidate)
		return candidate, nil
	}
	return Link{}, fmt.Errorf("no unique code after %d attempts", maxCodeAttempts)
}

// Resolve returns the target for a code, using the cache first.
//
// A missing code is negatively cached for a short window so enumeration cannot
// turn into a database query per request.
func (s *Service) Resolve(ctx context.Context, code string) (Link, error) {
	if !ValidCode(code) {
		return Link{}, ErrNotFound
	}

	if cached, ok, err := s.cache.Get(ctx, code); err != nil {
		s.log.Warn("cache read failed; falling back to database", "code", code, "err", err)
	} else if ok {
		if cached.ExpiresAt != nil && !s.now().Before(*cached.ExpiresAt) {
			_ = s.cache.Delete(ctx, code)
			return Link{}, ErrNotFound
		}
		return Link{Code: code, TargetURL: cached.TargetURL, ExpiresAt: cached.ExpiresAt}, nil
	}

	if miss, err := s.cache.GetMiss(ctx, code); err == nil && miss {
		return Link{}, ErrNotFound
	}

	l, err := s.store.GetByCode(ctx, code)
	if errors.Is(err, ErrNotFound) {
		if s.negativeTTL > 0 {
			if err := s.cache.SetMiss(ctx, code, s.negativeTTL); err != nil {
				s.log.Warn("negative cache write failed", "code", code, "err", err)
			}
		}
		return Link{}, ErrNotFound
	}
	if err != nil {
		return Link{}, fmt.Errorf("resolve link: %w", err)
	}
	if l.Expired(s.now()) {
		return Link{}, ErrNotFound
	}

	ttl := s.cacheTTLFor(l)
	if ttl > 0 {
		if err := s.cache.Set(ctx, code, Cached{TargetURL: l.TargetURL, ExpiresAt: l.ExpiresAt}, ttl); err != nil {
			s.log.Warn("cache write failed", "code", code, "err", err)
		}
	}
	return l, nil
}

// GetForUser returns one link owned by the caller. Another user's code is
// indistinguishable from a missing one.
func (s *Service) GetForUser(ctx context.Context, code string, userID uuid.UUID) (Link, error) {
	if !ValidCode(code) {
		return Link{}, ErrNotFound
	}
	l, err := s.store.GetByCodeForUser(ctx, code, userID)
	if errors.Is(err, ErrNotFound) {
		return Link{}, ErrNotFound
	}
	if err != nil {
		return Link{}, fmt.Errorf("get link: %w", err)
	}
	return l, nil
}

// List returns the caller's links, newest first.
func (s *Service) List(ctx context.Context, userID uuid.UUID, limit, offset int) ([]Link, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	links, err := s.store.ListByUser(ctx, userID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list links: %w", err)
	}
	return links, nil
}

// Delete soft-deletes a link and evicts it from the cache so the code stops
// resolving immediately.
func (s *Service) Delete(ctx context.Context, code string, userID uuid.UUID) error {
	if !ValidCode(code) {
		return ErrNotFound
	}
	if err := s.store.SoftDelete(ctx, code, userID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrNotFound
		}
		return fmt.Errorf("delete link: %w", err)
	}
	if err := s.cache.Delete(ctx, code); err != nil {
		s.log.Warn("cache eviction failed after delete", "code", code, "err", err)
	}
	return nil
}

// warm writes a freshly created link into the cache and clears any negative
// entry left by someone probing the alias before it existed.
func (s *Service) warm(ctx context.Context, l Link) {
	if err := s.cache.DeleteMiss(ctx, l.Code); err != nil {
		s.log.Warn("negative cache clear failed", "code", l.Code, "err", err)
	}
	ttl := s.cacheTTLFor(l)
	if ttl <= 0 {
		return
	}
	if err := s.cache.Set(ctx, l.Code, Cached{TargetURL: l.TargetURL, ExpiresAt: l.ExpiresAt}, ttl); err != nil {
		s.log.Warn("cache warm failed", "code", l.Code, "err", err)
	}
}

// cacheTTLFor never caches a link past its own expiry.
func (s *Service) cacheTTLFor(l Link) time.Duration {
	ttl := s.cacheTTL
	if l.ExpiresAt != nil {
		if d := l.ExpiresAt.Sub(s.now()); d < ttl {
			ttl = d
		}
	}
	return ttl
}
