package pgaudit

import "testing"

// No concurrent access tests here: Attribute is a pure function.

// Happy path: pg_sage's own role under an action tag is attributed to that
// decision, by the tag.
func TestAttributeActionTagFromOwnRole(t *testing.T) {
	attributeCase(t, "pg_sage action:77", ownRole, ownRole,
		Attribution{By: ByActionTag, DecisionID: 77}, true)
	if ByActionTag != "action_tag" {
		t.Fatalf("correlated_by value = %q", ByActionTag)
	}
}

// Invalid input: the tag names a decision only on pg_sage's own role. Any
// other role claiming it is not attributed (an agent role stays its own).
func TestAttributeActionTagNeedsOwnRole(t *testing.T) {
	for _, user := range []string{"app_user", "mallory", "Sage", "sage ", "postgres", ""} {
		attributeCase(t, "pg_sage action:77", user, ownRole, Attribution{}, false)
	}
	attributeCase(t, "pg_sage action:77", "sage_agent_k2m4q7x7ab", ownRole,
		Attribution{By: ByRole, Role: "sage_agent_k2m4q7x7ab"}, true)
	// Nil/empty: an unread own role matches nothing, not even an empty user.
	attributeCase(t, "pg_sage action:77", "", "", Attribution{}, false)
}

// Boundary: only an exact tag is one; a malformed one from pg_sage's role
// is neither a tag nor pg_sage's generic session.
func TestAttributeMalformedActionTag(t *testing.T) {
	for _, app := range []string{"pg_sage action:0", "pg_sage action:", "pg_sage action:x",
		"pg_sage action:-1", "pg_sage action:12 ", "pg_sage action:99999999999999999999"} {
		attributeCase(t, app, ownRole, ownRole, Attribution{}, false)
	}
}
