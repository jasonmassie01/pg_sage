package main

import "fmt"

// haIdentityKey is the stable history key of a monitored database: its
// meta-db record id when there is one, otherwise its instance name ("" when
// it has neither, which leaves persistence off).
func haIdentityKey(database string, databaseID *int) string {
	switch {
	case databaseID != nil:
		return fmt.Sprintf("db:%d", *databaseID)
	case database != "":
		return "name:" + database
	}
	return ""
}
