package mcpauth

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// maxDelegationDepth bounds how deep a nested act claim is followed.
const maxDelegationDepth = 8

// taskPattern is a task id: printable, no whitespace, at most 200 bytes.
var taskPattern = regexp.MustCompile(`^[\x21-\x7e]{1,200}$`)

// claims are the access-token claims pg_sage reads beyond go-oidc's.
type claims struct {
	Scope string                     `json:"scope"`
	Scp   json.RawMessage            `json:"scp"`
	Act   *actClaim                  `json:"act"`
	Extra map[string]json.RawMessage `json:"-"`
}

// actClaim is RFC 8693's actor claim, possibly nested.
type actClaim struct {
	Sub string    `json:"sub"`
	Iss string    `json:"iss"`
	Act *actClaim `json:"act"`
}

// UnmarshalJSON keeps every claim so a configured task claim can be read.
func (c *claims) UnmarshalJSON(b []byte) error {
	type plain claims
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	if err := json.Unmarshal(b, &p.Extra); err != nil {
		return err
	}
	*c = claims(p)
	return nil
}

// grantedScopes maps the token's scopes to MCP scopes. Read is required for
// anything; approve is never granted to a token (agents never approve).
func grantedScopes(c claims) []string {
	have := map[string]bool{}
	for _, s := range strings.Fields(c.Scope) {
		have[s] = true
	}
	var list []string
	if json.Unmarshal(c.Scp, &list) == nil {
		for _, s := range list {
			have[s] = true
		}
	} else {
		var one string
		if json.Unmarshal(c.Scp, &one) == nil {
			for _, s := range strings.Fields(one) {
				have[s] = true
			}
		}
	}
	if !have[ScopeRead] {
		return nil
	}
	out := []string{"read"}
	if have[ScopePropose] {
		out = append(out, "propose")
	}
	return out
}

// identity names the acting party. Under RFC 8693 delegation the token's
// sub is whom it acts for and act.sub is the actor: the agent a principal
// binds to.
func (v *Validator) identity(tok *oidc.IDToken, c claims) (Identity, error) {
	id := Identity{Issuer: tok.Issuer, Subject: tok.Subject}
	if c.Act != nil {
		if c.Act.Sub == "" {
			return Identity{}, fmt.Errorf("%w: act claim without sub", ErrInvalidToken)
		}
		if c.Act.Iss != "" && c.Act.Iss != tok.Issuer {
			return Identity{}, fmt.Errorf("%w: actor from another issuer", ErrInvalidToken)
		}
		id.Subject, id.OnBehalfOf = c.Act.Sub, tok.Subject
		for prior, depth := c.Act.Act, 0; prior != nil; prior, depth = prior.Act, depth+1 {
			if depth >= maxDelegationDepth || prior.Sub == "" {
				return Identity{}, fmt.Errorf("%w: delegation chain too deep or "+
					"incomplete", ErrInvalidToken)
			}
			id.DelegationChain = append(id.DelegationChain, prior.Sub)
		}
	}
	if id.Subject == "" {
		return Identity{}, fmt.Errorf("%w: no subject", ErrInvalidToken)
	}
	task, err := v.task(c)
	if err != nil {
		return Identity{}, err
	}
	id.TaskID = task
	return id, nil
}

// task reads the configured task claim: absent is fine, malformed is not
// (it is the only source of per-task taint, so it fails closed).
func (v *Validator) task(c claims) (string, error) {
	if v.taskClaim == "" {
		return "", nil
	}
	raw, ok := c.Extra[v.taskClaim]
	if !ok || string(raw) == "null" {
		return "", nil
	}
	var task string
	if err := json.Unmarshal(raw, &task); err != nil || !taskPattern.MatchString(task) {
		return "", fmt.Errorf("%w: %s claim must be a printable string of at most "+
			"200 bytes", ErrInvalidToken, v.taskClaim)
	}
	return task, nil
}
