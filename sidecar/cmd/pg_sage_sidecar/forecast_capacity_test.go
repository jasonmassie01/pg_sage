package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/forecaster"
)

// forecast-06: the forecaster and /api/v1/forecasts/growth resolve the
// disk capacity from the same provider source as the runway monitor
// (clouds-07), falling back to forecaster.disk_capacity_bytes.

// No concurrent access tests here: the resolver's concurrent reads are
// covered in the forecaster package with a recording fake.

func capacityRuntime(declared int64, cloud *cloudtel.Runtime) *databaseRuntime {
	cfg := config.DefaultConfig()
	cfg.Forecaster.DiskCapacityBytes = declared
	return &databaseRuntime{cfg: cfg, spec: databaseRuntimeSpec{Name: "orders"}, cloud: cloud}
}

func TestForecastCapacityWithoutTelemetryIsTheDeclaredValue(t *testing.T) {
	c := capacityRuntime(50*testGiB, nil).forecastCapacity()(context.Background())
	if c.Source != forecaster.CapacityDeclared || c.Bytes != float64(50*testGiB) || c.Managed {
		t.Fatalf("capacity = %+v, want declared 50 GiB, not managed", c)
	}
	c = capacityRuntime(0, nil).forecastCapacity()(context.Background())
	if c.Source != forecaster.CapacityUndeclared || c.Bytes != 0 {
		t.Fatalf("nothing declared = %+v, want undeclared", c)
	}
}

func TestForecastCapacityPrefersFreshProviderTelemetry(t *testing.T) {
	cloud, clock := storageRuntime(t, 100*testGiB, false)
	resolve := capacityRuntime(50*testGiB, cloud).forecastCapacity()
	c := resolve(context.Background())
	if c.Source != forecaster.CapacityProviderTelemetry ||
		c.Bytes != float64(100*testGiB) || !c.Managed {
		t.Fatalf("fresh = %+v, want provider 100 GiB", c)
	}
	clock.mu.Lock()
	clock.t = clock.t.Add(cloudtel.MaxPointAge + time.Minute)
	clock.mu.Unlock()
	c = resolve(context.Background())
	if c.Source != forecaster.CapacityDeclared || c.Bytes != float64(50*testGiB) || !c.Managed {
		t.Fatalf("stale = %+v, want the declared 50 GiB on a managed database", c)
	}
}

func TestForecastCapacityUnboundedStorageIgnoresTheDeclaredValue(t *testing.T) {
	cloud, _ := storageRuntime(t, 100*testGiB, true)
	c := capacityRuntime(50*testGiB, cloud).forecastCapacity()(context.Background())
	if c.Source != forecaster.CapacityUnbounded || c.Bytes != 0 || !c.Managed {
		t.Fatalf("auto-growing storage = %+v, want capacity_unbounded with no bytes", c)
	}
}

// Wiring lock: the production forecaster takes the runtime's provider
// capacity, and the fleet registration carries the growth resolver.
func TestForecasterAndInstanceTakeTheProviderCapacity(t *testing.T) {
	withCapacity := 0
	for _, call := range productionCalls(t) {
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "WithCapacity" {
			continue
		}
		withCapacity++
		if len(call.Args) != 1 || types.ExprString(call.Args[0]) != "rt.runwayCapacity()" {
			t.Errorf("WithCapacity(%v), want rt.runwayCapacity()", call.Args)
		}
	}
	if withCapacity != 1 {
		t.Fatalf("%d WithCapacity calls, want the one in newForecaster", withCapacity)
	}
	if got := instanceField(t, "DiskCapacity"); got != "rt.forecastCapacity()" {
		t.Fatalf("fleet registration DiskCapacity = %q, want rt.forecastCapacity()", got)
	}
}

// instanceField is the value of key in rt.instance()'s fleet.DatabaseInstance.
func instanceField(t *testing.T, key string) string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "database_runtime.go", nil, 0)
	if err != nil {
		t.Fatalf("parse database_runtime.go: %v", err)
	}
	got := ""
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "instance" {
			return true
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			if kv, ok := n.(*ast.KeyValueExpr); ok && types.ExprString(kv.Key) == key {
				got = types.ExprString(kv.Value)
			}
			return true
		})
		return false
	})
	return got
}
