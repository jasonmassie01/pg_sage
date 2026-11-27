package basic

type db interface{ Exec() }

type svc struct{ db db }

// Run is production code. It calls Exec, which is also the name of a test
// mock's failing method, but production code cannot fail a test.
func (s *svc) Run() { s.db.Exec() }
