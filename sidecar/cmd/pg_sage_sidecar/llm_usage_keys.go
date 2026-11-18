package main

import (
	"strconv"
	"strings"
	"sync"
)

// llmUsageProcessKey names the process-wide clients and the global
// ceiling in sage.llm_usage.
const llmUsageProcessKey = "process"

// llmUsageKeys maps a database name to its sage.llm_usage key and back. A
// meta-DB database is keyed id:<sage.databases id>, so its row survives a
// rename; a YAML or standalone database is keyed name:<name>.
type llmUsageKeys struct {
	mu  sync.Mutex
	ids map[string]int
}

func newLLMUsageKeys() *llmUsageKeys { return &llmUsageKeys{ids: map[string]int{}} }

// llmUsageIDs holds the meta-DB ids of the running process.
var llmUsageIDs = newLLMUsageKeys()

// setID records a meta-DB database's id; a non-positive id is ignored.
// An id seen before under another name follows the rename.
func (k *llmUsageKeys) setID(name string, id int) {
	if id <= 0 {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	for known, knownID := range k.ids {
		if knownID == id {
			delete(k.ids, known) // renamed: the id names one database
		}
	}
	k.ids[name] = id
}

func (k *llmUsageKeys) keyOf(database string) string {
	if database == "" {
		return llmUsageProcessKey
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if id := k.ids[database]; id > 0 {
		return "id:" + strconv.Itoa(id)
	}
	return "name:" + database
}

// nameOf resolves a database key to the database it names today.
func (k *llmUsageKeys) nameOf(key string) (string, bool) {
	if name, ok := strings.CutPrefix(key, "name:"); ok {
		return name, name != ""
	}
	raw, ok := strings.CutPrefix(key, "id:")
	if !ok {
		return "", false
	}
	id, err := strconv.Atoi(raw)
	if err != nil || id <= 0 {
		return "", false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	for name, known := range k.ids {
		if known == id {
			return name, true
		}
	}
	return "", false
}

func llmUsageRole(role llmClientRole) string { return role.String() }
