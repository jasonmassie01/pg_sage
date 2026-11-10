package notests

import "testing"

func TestMain(m *testing.M) { m.Run() }

func helper(t *testing.T) { t.Log("unused") }
