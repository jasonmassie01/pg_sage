package clone

import (
	"context"
	"time"
)

type CloneSpec struct {
	IncludeData         bool  `json:"include_data"`
	TargetSizeHintBytes int64 `json:"target_size_hint_bytes"`
	// ResourceName is the name the provider gives the clone's resource
	// when it names resources (a Finder looks it up by this name); set from
	// the receipt by ReceiptingProvider. Never sent as a request field.
	ResourceName string `json:"-"`
}

type Clone struct {
	DSN         string    `json:"dsn"`
	ID          string    `json:"id"`
	CreatedFrom time.Time `json:"created_from"`
}

type Provider interface {
	Create(context.Context, CloneSpec) (Clone, error)
	Destroy(context.Context, Clone) error
	SnapshotAge(context.Context) (time.Duration, error)
}

// SharedTarget is a provider whose every Create hands out the same
// target (the local game-day database), so runs on it must take turns.
// TargetKey names that target without credentials. A provider that gives
// each run its own clone does not implement it.
type SharedTarget interface {
	TargetKey() string
}
