package config

// replaceRetiredSectionsForTest empties the registry and returns a function
// that restores it.
func replaceRetiredSectionsForTest() func() {
	retiredSectionsMu.Lock()
	saved := retiredSections
	retiredSections = map[string]RetiredSection{}
	retiredSectionsMu.Unlock()
	return func() {
		retiredSectionsMu.Lock()
		retiredSections = saved
		retiredSectionsMu.Unlock()
	}
}
