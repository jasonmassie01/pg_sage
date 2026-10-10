package mcp

import (
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// slowFirstReadDirectory is a fleet directory whose first read (the tool
// list watcher's baseline) is held until the fleet changes or holdFor
// passes, the way a goroutine the scheduler starts late (-race, a loaded
// CI runner) takes its first look late.
type slowFirstReadDirectory struct {
	*fakeDirectory
	once    sync.Once
	changed chan struct{}
	holdFor time.Duration
}

func (d *slowFirstReadDirectory) Databases() []DatabaseRef {
	d.once.Do(func() {
		select {
		case <-d.changed:
		case <-time.After(d.holdFor):
		}
	})
	return d.fakeDirectory.Databases()
}

func (d *slowFirstReadDirectory) set(refs ...DatabaseRef) {
	d.fakeDirectory.set(refs...)
	close(d.changed)
}

// A fleet change made once the subscription is acknowledged is notified
// even when the watcher's first look comes after it: the baseline the
// change is measured against is the tool list when Serve started, not
// whenever the watcher goroutine first ran (CI, PG14 -race: "no message on
// the stdio stream" in TestStdioModernSubscription).
func TestStdioChangeBeforeTheWatchersFirstLookIsNotified(t *testing.T) {
	dir := &slowFirstReadDirectory{fakeDirectory: fleetOf("orders"),
		changed: make(chan struct{}), holdFor: 300 * time.Millisecond}
	peer := startStdio(t, NewServer(&recordingBackend{}).WithDirectory(dir))
	peer.send(`{"jsonrpc":"2.0","id":"sub-2","method":"subscriptions/listen","params":{` +
		modernMeta + `,"notifications":{"toolsListChanged":true}}}`)
	require.Equal(t, "notifications/subscriptions/acknowledged", peer.next()["method"])
	dir.set(DatabaseRef{Name: "billing", ID: 2})
	changed := peer.next()
	require.Equal(t, "notifications/tools/list_changed", changed["method"])
	meta := objectMap(t, objectMap(t, changed["params"])["_meta"])
	require.Equal(t, "sub-2", meta["io.modelcontextprotocol/subscriptionId"])
	peer.quiet(50 * time.Millisecond)
}
