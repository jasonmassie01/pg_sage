package cases

// The package's internal tests project findings with the executor's own
// classifier, the one production passes. cases cannot import the executor
// (the executor depends on cases), so the external test package injects
// it from its init (classifier_inject_test.go), before any test runs.
var injectedSQLClassifier SQLClassifier

// SetTestSQLClassifier installs the classifier the internal tests use.
func SetTestSQLClassifier(classify SQLClassifier) { injectedSQLClassifier = classify }

// testClassify is the injected executor classifier; a test binary without
// it fails loudly instead of projecting with no classification.
func testClassify(sql string) (string, string) {
	if injectedSQLClassifier == nil {
		panic("cases tests need the executor classifier injected by cases_test")
	}
	return injectedSQLClassifier(sql)
}

// projectFinding projects f as the cases handler does.
func projectFinding(f SourceFinding) Case { return ProjectFinding(f, testClassify) }
