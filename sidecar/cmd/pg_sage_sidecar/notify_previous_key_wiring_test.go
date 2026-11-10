package main

import (
	"go/ast"
	"testing"
)

// security-08 review: every channel-secret reader (the API's stores and
// dispatcher, and the runtime dispatchers) must also hold the previous key
// of a rotation. YAML fleet mode has no startup re-seal, so without it a
// rotation left every sealed channel secret unreadable.

func TestWireRouterPassesPreviousNotificationKey(t *testing.T) {
	wire := productionFunction(t, "wireRouter")
	if !compositeFields(wire, "RuntimeDeps")["NotificationPreviousSecretKey"] {
		t.Fatal("wireRouter RuntimeDeps omits NotificationPreviousSecretKey")
	}
	if !callsIdentifier(wire, "notificationPreviousSecretKey") {
		t.Fatal("wireRouter does not derive the previous key for the auth pool")
	}
}

func TestSharedDispatcherOpensPreviousKey(t *testing.T) {
	fn := productionFunction(t, "sharedNotifyDispatcher")
	if !callsMethod(fn, "WithPreviousKey") ||
		!callsIdentifier(fn, "notificationPreviousSecretKey") {
		t.Fatal("the runtime dispatcher's PoolStore does not get the previous key")
	}
}

func TestNotificationPreviousSecretKeyWithoutKeys(t *testing.T) {
	// nil pool: no control database, so no key of either kind.
	if got := notificationPreviousSecretKey(nil); got != nil {
		t.Fatalf("previous key for a nil pool = %x, want nil", got)
	}
}

func callsMethod(fn *ast.FuncDecl, name string) bool {
	found := false
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok {
			if sel, isSel := call.Fun.(*ast.SelectorExpr); isSel && sel.Sel.Name == name {
				found = true
			}
		}
		return !found
	})
	return found
}
