package clone

import (
	"context"
	"time"
)

type CloneSpec struct {
	IncludeData         bool  `json:"include_data"`
	TargetSizeHintBytes int64 `json:"target_size_hint_bytes"`
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
