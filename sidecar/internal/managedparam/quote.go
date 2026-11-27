package managedparam

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrUnsafeProviderValue is why a command was withheld: a name or value
// read from the provider cannot be quoted safely into it.
var ErrUnsafeProviderValue = errors.New("a provider value cannot be quoted safely")

// gcloudDelimiters are the alternate dictionary delimiters tried, in
// order, when a flag value contains a comma (`gcloud topic escaping`).
const gcloudDelimiters = "@:|~"

// shellQuote quotes s as one POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// checkProviderValue reports, naming the setting, a provider value that
// may not reach a command.
func checkProviderValue(name, value string) error {
	if !parameterPattern.MatchString(name) {
		return fmt.Errorf("%w: the provider reports a setting named %.64q, which is not a "+
			"valid parameter name", ErrUnsafeProviderValue, name)
	}
	if !providerValuePattern.MatchString(value) {
		return fmt.Errorf("%w: the provider's value of %s contains whitespace, a quote, a "+
			"shell metacharacter or a control character, or is longer than 256 characters",
			ErrUnsafeProviderValue, name)
	}
	return nil
}

// gcloudDict renders flags as one gcloud dictionary argument value. A
// value with a comma switches to the ^DELIM^ syntax with a delimiter no
// value contains.
func gcloudDict(flags map[string]string) (string, error) {
	keys := make([]string, 0, len(flags))
	for k := range flags {
		keys = append(keys, k)
	}
	sort.Strings(keys) // checked in order, so the first unsafe flag is named
	needsDelim := false
	for _, k := range keys {
		if err := checkProviderValue(k, flags[k]); err != nil {
			return "", err
		}
		needsDelim = needsDelim || strings.Contains(flags[k], ",")
	}
	sep, prefix := ",", ""
	if needsDelim {
		var err error
		if sep, err = pickDelimiter(flags); err != nil {
			return "", err
		}
		prefix = "^" + sep + "^"
	}
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+flags[k])
	}
	return prefix + strings.Join(pairs, sep), nil
}

func pickDelimiter(flags map[string]string) (string, error) {
	for _, d := range strings.Split(gcloudDelimiters, "") {
		free := true
		for _, v := range flags {
			free = free && !strings.Contains(v, d)
		}
		if free {
			return d, nil
		}
	}
	names := make([]string, 0, len(flags))
	for k, v := range flags {
		if strings.Contains(v, ",") {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	return "", fmt.Errorf("%w: the values of %s contain a comma and every gcloud delimiter "+
		"(%s), so the flag list cannot be escaped", ErrUnsafeProviderValue,
		strings.Join(names, ", "), gcloudDelimiters)
}

// rdsParameter is one entry of the aws rds --parameters JSON list.
type rdsParameter struct {
	ParameterName  string `json:"ParameterName"`
	ParameterValue string `json:"ParameterValue,omitempty"`
	ApplyMethod    string `json:"ApplyMethod"`
}

// rdsParametersArg renders one parameter as the quoted JSON form of
// --parameters, which carries commas and other characters that break the
// shorthand syntax.
func rdsParametersArg(name, value, method string) string {
	// Marshalling a struct of strings cannot fail.
	raw, _ := json.Marshal([]rdsParameter{{ParameterName: name, ParameterValue: value,
		ApplyMethod: method}})
	return shellQuote(string(raw))
}
