package agenttools

import (
	"encoding/json"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// No concurrent access tests in this file: QueryID's JSON methods are pure
// functions of their input and share no state.

type queryIDHolder struct {
	ID QueryID `json:"id"`
}

func TestQueryIDUnmarshalNumberAndString(t *testing.T) {
	cases := map[string]QueryID{
		`{"id":123}`:                    123,
		`{"id":-5}`:                     -5,
		`{"id":"42"}`:                   42,
		`{"id":"-8765432109876543210"}`: QueryID(-8765432109876543210),
		// Beyond float64's 53-bit mantissa: must not round through float64.
		`{"id":8765432109876543210}`:   QueryID(8765432109876543210),
		`{"id":-9223372036854775808}`:  QueryID(-9223372036854775808),
		`{"id":"9223372036854775807"}`: QueryID(9223372036854775807),
	}
	for raw, want := range cases {
		var h queryIDHolder
		require.NoError(t, json.Unmarshal([]byte(raw), &h), raw)
		require.Equal(t, want, h.ID, raw)
	}
}

func TestQueryIDUnmarshalRejectsInvalid(t *testing.T) {
	for _, raw := range []string{
		`{"id":"abc"}`, `{"id":"12x"}`, `{"id":""}`, `{"id":"1.5"}`, `{"id":1.5}`,
		`{"id":"99999999999999999999"}`, `{"id":99999999999999999999}`,
		`{"id":true}`, `{"id":{}}`, `{"id":[1]}`, `{"id":"1e3"}`,
	} {
		var h queryIDHolder
		err := json.Unmarshal([]byte(raw), &h)
		require.Error(t, err, "accepted %s as %d", raw, h.ID)
	}
}

func TestQueryIDMarshalsAsString(t *testing.T) {
	out, err := json.Marshal(QueryID(-8765432109876543210))
	require.NoError(t, err)
	require.Equal(t, `"-8765432109876543210"`, string(out))
	out, err = json.Marshal(queryIDHolder{ID: 7})
	require.NoError(t, err)
	require.JSONEq(t, `{"id":"7"}`, string(out))
	// TopQuery exposes it under "queryid", as a string.
	out, err = json.Marshal(TopQuery{QueryID: 8765432109876543210})
	require.NoError(t, err)
	require.Contains(t, string(out), `"queryid":"8765432109876543210"`)
	// Round trip keeps every bit.
	var back queryIDHolder
	require.NoError(t, json.Unmarshal([]byte(`{"id":"-8765432109876543210"}`), &back))
	again, err := json.Marshal(back)
	require.NoError(t, err)
	require.JSONEq(t, `{"id":"-8765432109876543210"}`, string(again))
}
