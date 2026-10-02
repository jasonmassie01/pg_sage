package collector

import "context"

// catalogStep reads one snapshot category.
type catalogStep struct {
	name string
	run  func(context.Context, *Snapshot) error
}

// catalogSteps lists the catalog categories collect reads, in order. A
// test may override one through overrideStep.
func (c *Collector) catalogSteps() []catalogStep {
	steps := []catalogStep{
		{"queries", func(ctx context.Context, s *Snapshot) (err error) {
			s.Queries, err = c.collectQueries(ctx)
			return err
		}},
		{"tables", func(ctx context.Context, s *Snapshot) (err error) {
			s.Tables, err = c.collectTables(ctx)
			return err
		}},
		{"indexes", func(ctx context.Context, s *Snapshot) (err error) {
			s.Indexes, err = c.collectIndexes(ctx)
			return err
		}},
		{"foreign_keys", func(ctx context.Context, s *Snapshot) (err error) {
			s.ForeignKeys, err = c.collectForeignKeys(ctx)
			return err
		}},
		{"system", func(ctx context.Context, s *Snapshot) (err error) {
			s.System, err = c.collectSystem(ctx)
			return err
		}},
		{"locks", func(ctx context.Context, s *Snapshot) (err error) {
			s.Locks, err = c.collectLocks(ctx)
			return err
		}},
		{"sequences", func(ctx context.Context, s *Snapshot) (err error) {
			s.Sequences, err = c.collectSequences(ctx)
			return err
		}},
	}
	for i, st := range steps {
		if fn, ok := c.stepOverrides[st.name]; ok {
			steps[i].run = fn
		}
	}
	return steps
}

// overrideStep replaces one category's reader (tests).
func (c *Collector) overrideStep(name string, fn func(context.Context, *Snapshot) error) {
	if c.stepOverrides == nil {
		c.stepOverrides = map[string]func(context.Context, *Snapshot) error{}
	}
	c.stepOverrides[name] = fn
}
