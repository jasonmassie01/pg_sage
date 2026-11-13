package gameday

import (
	"encoding/json"
	"fmt"
)

// Exercised is what a game day exercised (benches-12): the clone
// provider, whether its clone holds customer objects, and each scenario
// of the run's report, a customer program with its binding (the quoted
// table name, local row only, and its hash) or its skip reason.
type Exercised struct {
	Provider        string              `json:"provider"`
	CustomerObjects bool                `json:"customer_objects"`
	Scenarios       []ExercisedScenario `json:"scenarios"`
}

// ExercisedScenario is one scenario's runs in the report. Skip is the
// first skip reason of a scenario that scored no run; Error the first
// error of an errored one.
type ExercisedScenario struct {
	ID          string `json:"id"`
	Runs        int    `json:"runs"`
	Scored      int    `json:"scored"`
	Binding     string `json:"binding,omitempty"`
	BindingHash string `json:"binding_hash,omitempty"`
	Skip        string `json:"skip,omitempty"`
	Error       string `json:"error,omitempty"`
}

// customerPrograms are the scenario ids that exercise customer tables.
var customerPrograms = map[string]bool{ProgramCustomerHotTable: true}

// exercisedOf reads raw (a bench report, possibly nil or unreadable) into
// what the run exercised. Every binding is listed, run or not.
func exercisedOf(provider string, bindings []Binding, raw []byte) Exercised {
	x := Exercised{Provider: provider, CustomerObjects: HoldsCustomerObjects(provider),
		Scenarios: []ExercisedScenario{}}
	index := map[string]int{}
	for _, run := range reportRuns(raw) {
		i, ok := index[run.Scenario]
		if !ok {
			i = len(x.Scenarios)
			index[run.Scenario] = i
			x.Scenarios = append(x.Scenarios, ExercisedScenario{ID: run.Scenario})
		}
		sc := &x.Scenarios[i]
		sc.Runs++
		switch {
		case run.Skipped != "":
			if sc.Skip == "" {
				sc.Skip = truncate(run.Skipped, maxSkipReason)
			}
		case run.Error != "":
			if sc.Error == "" {
				sc.Error = truncate(run.Error, maxSkipReason)
			}
		default:
			sc.Scored++
		}
	}
	for _, b := range bindings {
		i, ok := index[b.Program]
		if !ok {
			i = len(x.Scenarios)
			index[b.Program] = i
			x.Scenarios = append(x.Scenarios, ExercisedScenario{ID: b.Program})
		}
		sc := &x.Scenarios[i]
		sc.Binding, sc.BindingHash = b.Ident(), b.Hash()
		if b.Skip != "" {
			sc.Skip = b.Skip
		}
	}
	for i := range x.Scenarios {
		if x.Scenarios[i].Scored > 0 {
			x.Scenarios[i].Skip = ""
		}
	}
	return x
}

type reportRun struct {
	Scenario string `json:"scenario"`
	Skipped  string `json:"skipped"`
	Error    string `json:"error"`
}

func reportRuns(raw []byte) []reportRun {
	var report struct {
		Runs []reportRun `json:"runs"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &report) != nil {
		return nil
	}
	return report.Runs
}

// customerUnscored is the first customer program the report ran that
// scored no run (it skipped or errored): the game day then says nothing
// about the customer's database. A program absent from the report (its
// family was not selected) is not one.
func customerUnscored(x Exercised) (ExercisedScenario, bool) {
	for _, sc := range x.Scenarios {
		if customerPrograms[sc.ID] && sc.Runs > 0 && sc.Scored == 0 {
			return sc, true
		}
	}
	return ExercisedScenario{}, false
}

// customerFailure is an error when a customer program's run errored: a
// broken program on a customer clone fails the game day.
func customerFailure(x Exercised) error {
	for _, sc := range x.Scenarios {
		if customerPrograms[sc.ID] && sc.Error != "" {
			return fmt.Errorf("customer program %s: %s", sc.ID, sc.Error)
		}
	}
	return nil
}
