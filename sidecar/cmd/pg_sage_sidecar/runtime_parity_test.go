package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// runtimeCapability is one per-database component or setting. Any listed
// call satisfies it; a trailing "*" matches a prefix.
type runtimeCapability struct {
	name  string
	calls []string
}

// perDatabaseCapabilities is what every monitored database gets, whatever
// the deployment mode (G5-I07). The four modes drifted apart because each
// wired these by hand.
var perDatabaseCapabilities = []runtimeCapability{
	{"prerequisite checks", []string{"runInstanceChecks", "instanceChecksOrDegraded",
		"startup.RunChecks"}},
	{"schema bootstrap", []string{"schema.Bootstrap", "bootstrapManagedDatabaseSchema"}},
	{"trust ramp start", []string{"schema.PersistTrustRampStart"}},
	{"collector", []string{"collector.New"}},
	{"analyzer", []string{"analyzer.New"}},
	{"runaway detector", []string{"executor.NewRunawayDetector"}},
	{"forecaster", []string{"forecaster.New"}},
	{"query tuner", []string{"tuner.New"}},
	{"hint revalidation loop", []string{".StartRevalidationLoop"}},
	{"index optimizer", []string{"optimizer.New"}},
	{"optimizer auto_explain plans", []string{"optimizer.WithAutoExplain"}},
	{"config advisor", []string{"advisor.New"}},
	{"advisor cloud environment", []string{".WithCloudEnv"}},
	{"auto_explain collector", []string{"autoexplain.NewCollector"}},
	{"RCA engine", []string{"rca.NewEngine"}},
	{"log watcher", []string{"logwatch.NewFileWatcher"}},
	{"plan narrator", []string{"analyzer.NewLLMPlanNarrator"}},
	{"notification dispatcher", []string{"sharedNotifyDispatcher"}},
	{"alerting", []string{"newInstanceAlertManager"}},
	{"executor", []string{"executor.New"}},
	{"ANALYZE semaphore", []string{".WithAnalyzeSemaphore"}},
	{"managed-config adapter", []string{"installAzureManagedConfig"}},
	{"provider observability", []string{"startProviderObservability"}},
	{"action store", []string{"store.NewActionStore"}},
	{"action expiry", []string{"store.StartActionExpiry"}},
	{"standing policy gate", []string{".EnableStandingPolicy*"}},
	{"per-database trust level", []string{".SetTrustLevel"}},
	{"per-database executor gate", []string{".SetExecutorEnabled"}},
	{"action justifier", []string{".WithJustifier"}},
	{"continuous autonomy", []string{"newDatabaseAutonomy"}},
	{"briefing", []string{"briefing.New"}},
	{"retention", []string{"retention.New"}},
	{"schema lint", []string{"lint.NewRunner"}},
	{"migration advisor", []string{"migration.NewAdvisor"}},
	{"migration log detection", []string{"migration.NewLogDetector"}},
	{"HA-gated orchestrator", []string{"ha.New"}},
	// Overnight features (D6, Sage SRE M0) that every mode must carry.
	{"D6 pg-side IO load admission", []string{"startIOAdmission"}},
	{"D6 per-database IO capacity", []string{"databaseExecConfig"}},
	{"Sage SRE registration (rcaAdapter)", []string{"newRCAAdapter"}},
	{"Sage SRE lock-chain fast path", []string{"rca.NewLockChainTicker"}},
	{"Sage SRE catalog probes", []string{"probes.NewRunner"}},
	{"lifecycle-owned workers", []string{"startInstanceWorker"}},
}

// runtimeModeEntries are the functions that build one database's runtime.
var runtimeModeEntries = map[string]string{
	"standalone": "initStandalone",
	"yaml-fleet": "initFleetMultiDB",
	"meta-db":    "prepareStoreDatabaseConnection",
}

func TestRuntimeParityEveryModeWiresEveryCapability(t *testing.T) {
	graph := loadPackageCallGraph(t)
	modes := make([]string, 0, len(runtimeModeEntries))
	for mode := range runtimeModeEntries {
		modes = append(modes, mode)
	}
	sort.Strings(modes)
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			reached := graph.reachableCalls(runtimeModeEntries[mode])
			if len(reached) == 0 {
				t.Fatalf("entry %s reaches no calls", runtimeModeEntries[mode])
			}
			var missing []string
			for _, capability := range perDatabaseCapabilities {
				if !capability.satisfiedBy(reached) {
					missing = append(missing, capability.name)
				}
			}
			if len(missing) > 0 {
				t.Errorf("%s runtime lacks: %s", mode, strings.Join(missing, ", "))
			}
		})
	}
}

// The helper must see through local functions and methods, so a mode that
// delegates to a shared constructor is credited with what it builds.
func TestRuntimeCallGraphFollowsLocalCalls(t *testing.T) {
	graph := &packageCallGraph{
		funcs: map[string][]*ast.FuncDecl{}, methods: map[string][]*ast.FuncDecl{},
		imports: map[string]bool{"collector": true},
	}
	source := `package main
func entry() { helper() }
func helper() { var r runner; r.build() }
type runner struct{}
func (runner) build() { collector.New() }
func unrelated() { executor.New() }`
	parsed, err := parser.ParseFile(token.NewFileSet(), "x.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	graph.index(parsed)
	reached := graph.reachableCalls("entry")
	if !reached["collector.New"] {
		t.Fatalf("call through a local method was not followed: %v", reached)
	}
	if reached["executor.New"] {
		t.Fatal("unreachable function credited to the entry point")
	}
	if len(graph.reachableCalls("missing")) != 0 {
		t.Fatal("unknown entry point reported calls")
	}
}

func (c runtimeCapability) satisfiedBy(reached map[string]bool) bool {
	for _, call := range c.calls {
		prefix, isPrefix := strings.CutSuffix(call, "*")
		for name := range reached {
			if name == call || (isPrefix && strings.HasPrefix(name, prefix)) {
				return true
			}
		}
	}
	return false
}

type packageCallGraph struct {
	funcs   map[string][]*ast.FuncDecl
	methods map[string][]*ast.FuncDecl
	imports map[string]bool
}

func loadPackageCallGraph(t *testing.T) *packageCallGraph {
	t.Helper()
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source directory")
	}
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(current), "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	graph := &packageCallGraph{
		funcs: map[string][]*ast.FuncDecl{}, methods: map[string][]*ast.FuncDecl{},
		imports: map[string]bool{},
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, source, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		graph.index(parsed)
	}
	return graph
}

func (g *packageCallGraph) index(file *ast.File) {
	for _, spec := range file.Imports {
		name := strings.Trim(spec.Path.Value, `"`)
		name = name[strings.LastIndex(name, "/")+1:]
		if spec.Name != nil {
			name = spec.Name.Name
		}
		g.imports[name] = true
	}
	for _, declaration := range file.Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if fn.Recv != nil {
			g.methods[fn.Name.Name] = append(g.methods[fn.Name.Name], fn)
			continue
		}
		g.funcs[fn.Name.Name] = append(g.funcs[fn.Name.Name], fn)
	}
}

// reachableCalls returns every call name reachable from entry: "pkg.Func"
// for imported packages, ".Method" for other selectors and the bare name
// for local identifiers.
func (g *packageCallGraph) reachableCalls(entry string) map[string]bool {
	reached := map[string]bool{}
	visited := map[*ast.FuncDecl]bool{}
	queue := append([]*ast.FuncDecl(nil), g.funcs[entry]...)
	for len(queue) > 0 {
		fn := queue[0]
		queue = queue[1:]
		if visited[fn] {
			continue
		}
		visited[fn] = true
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if ok {
				queue = append(queue, g.visitCall(call, reached)...)
			}
			return true
		})
	}
	return reached
}

func (g *packageCallGraph) visitCall(
	call *ast.CallExpr, reached map[string]bool,
) []*ast.FuncDecl {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		reached[fun.Name] = true
		return g.funcs[fun.Name]
	case *ast.SelectorExpr:
		if pkg, ok := fun.X.(*ast.Ident); ok && g.imports[pkg.Name] {
			reached[pkg.Name+"."+fun.Sel.Name] = true
			return nil
		}
		reached["."+fun.Sel.Name] = true
		return g.methods[fun.Sel.Name]
	}
	return nil
}
