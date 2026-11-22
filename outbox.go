package favorites

import (
	"context"
	"encoding/json"
	"time"
)

const OutboxChannel = "favorites_outbox"

// OutboxReader is a trusted relay port. The relay serializes each source/subject
// and confirms only after every subscribed consumer durably accepts the event.
// Exclude up to 1000 subjects currently backing off or awaiting manual handling.
type OutboxReader interface {
	PendingOutbox(context.Context, int, []string) ([]Envelope, error)
	ConfirmOutbox(context.Context, string) error
}

// OutboxLag summarizes unsent outbox backlog for relay health checks and metrics.
type OutboxLag struct {
	PendingCount    int64
	OldestPendingAt time.Time
	OldestAge       time.Duration
}

type OutboxObserver interface {
	OutboxLag(context.Context) (OutboxLag, error)
}

func (s *Service) OutboxLag(ctx context.Context) (OutboxLag, error) {
	if s.Repo == nil {
		return OutboxLag{}, ErrUnavailable
	}
	obs, ok := s.Repo.(OutboxObserver)
	if !ok {
		return OutboxLag{}, ErrUnavailable
	}
	lag, e := obs.OutboxLag(ctx)
	if e != nil || lag.PendingCount == 0 || s.Now == nil {
		return lag, e
	}
	lag.OldestAge = max(0, s.now().Sub(lag.OldestPendingAt))
	return lag, nil
}

// Envelope carries either a discovery document or the minimal business revision.
// Consumers select a payload type using Type; no private persistence model is sent.
type Envelope struct {
	SpecVersion string          `json:"specversion"`
	ID          string          `json:"id"`
	Source      string          `json:"source"`
	Type        string          `json:"type"`
	Subject     string          `json:"subject"`
	Time        time.Time       `json:"time"`
	Data        json.RawMessage `json:"data"`
}
