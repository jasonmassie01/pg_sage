package envbind

import (
	"context"
	"time"
)

// Resolver maps a fleet database name to its Database (ErrUnknownDatabase
// when there is none).
type Resolver func(ctx context.Context, name string) (Database, error)

// Service serves environment labels by database name (REST, §8.3).
type Service struct {
	Binder  *Binder
	Resolve Resolver
}

// Pending is a widening label waiting for a second admin.
type Pending struct {
	Label       Env        `json:"label"`
	RequestedBy string     `json:"requested_by"`
	RequestedAt *time.Time `json:"requested_at,omitempty"`
}

// View is a database's label as served: the stored label and its identity
// snapshot, and what it evaluates to now.
type View struct {
	Database  string     `json:"database"`
	Label     Env        `json:"label"`
	Effective Env        `json:"effective"`
	Verified  bool       `json:"verified"`
	Identity  Identity   `json:"identity"`
	Live      Identity   `json:"live"`
	SetBy     string     `json:"set_by,omitempty"`
	SetAt     *time.Time `json:"set_at,omitempty"`
	Reasons   []string   `json:"reasons"`
	Critical  bool       `json:"critical"`
	Changed   []string   `json:"changed,omitempty"`
	Conflicts []Peer     `json:"conflicts,omitempty"`
	Pending   *Pending   `json:"pending,omitempty"`
}

func (s *Service) database(ctx context.Context, name string) (Database, error) {
	if s == nil || s.Binder == nil || s.Resolve == nil {
		return Database{}, ErrNoControl
	}
	return s.Resolve(ctx, name)
}

// View evaluates a database now.
func (s *Service) View(ctx context.Context, name string) (View, error) {
	db, err := s.database(ctx, name)
	if err != nil {
		return View{}, err
	}
	bind, err := s.Binder.EnvironmentOf(ctx, db)
	if err != nil {
		return View{}, err
	}
	v := viewOf(bind.Evidence)
	if db.ID == "" || s.Binder.control == nil {
		return v, nil
	}
	rec, ok, err := s.Binder.Record(ctx, db.ID)
	if err != nil {
		return View{}, err
	}
	if ok && rec.PendingLabel != "" {
		v.Pending = &Pending{Label: rec.PendingLabel, RequestedBy: rec.PendingBy,
			RequestedAt: rec.PendingAt}
	}
	return v, nil
}

func viewOf(ev Evidence) View {
	v := View{Database: ev.Database, Label: ev.Label, Effective: ev.Effective,
		Verified: ev.Verified, Identity: ev.Live, Live: ev.Live, SetBy: ev.SetBy,
		SetAt: ev.SetAt, Reasons: ev.Reasons, Critical: ev.Critical, Changed: ev.Changed,
		Conflicts: ev.Conflicts}
	if ev.Snapshot != nil {
		v.Identity = *ev.Snapshot
	}
	return v
}

// SetLabel sets a database's label as actor and returns the result with
// the database's view afterwards.
func (s *Service) SetLabel(ctx context.Context, name string, label Env, actor string) (
	SetResult, View, error) {
	db, err := s.database(ctx, name)
	if err != nil {
		return SetResult{}, View{}, err
	}
	res, err := s.Binder.SetLabel(ctx, db, label, actor)
	if err != nil {
		return res, View{}, err
	}
	v, err := s.View(ctx, name)
	return res, v, err
}
