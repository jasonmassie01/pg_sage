package notify

import (
	"context"
	"fmt"
	"strconv"
)

var _ ReconcileStore = (*MemoryStore)(nil)

// InReconcileTx runs fn under the store's lock on the live rows and puts
// the channels, rules and hashes back when fn fails, like a rolled-back
// transaction.
func (m *MemoryStore) InReconcileTx(ctx context.Context, fn func(ReconcileTx) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	channels := make(map[int]Channel, len(m.channels))
	for id, ch := range m.channels {
		channels[id] = ch
	}
	rules := append([]Rule(nil), m.rules...)
	hashes := make(map[string]string, len(m.hashes))
	for k, v := range m.hashes {
		hashes[k] = v
	}
	chSeq, ruleSeq := m.channelSeq, m.ruleSeq
	if err := fn(memReconcileTx{m}); err != nil {
		m.channels, m.rules, m.hashes = channels, rules, hashes
		m.channelSeq, m.ruleSeq = chSeq, ruleSeq
		return err
	}
	return nil
}

// memReconcileTx works on the store while InReconcileTx holds its lock.
type memReconcileTx struct{ m *MemoryStore }

func channelHashKey(id int) string { return "c" + strconv.Itoa(id) }
func ruleHashKey(id int) string    { return "r" + strconv.Itoa(id) }

func (t memReconcileTx) fail(name string) error {
	if t.m.FailReconcile != nil && t.m.FailReconcileAt == name {
		return t.m.FailReconcile
	}
	return nil
}

func (t memReconcileTx) ManagedChannels(context.Context) ([]ManagedChannel, error) {
	var out []ManagedChannel
	for id := 1; id <= t.m.channelSeq; id++ {
		ch, ok := t.m.channels[id]
		if ok && ch.ManagedBy == ManagedByConfig {
			out = append(out, ManagedChannel{ID: id, Name: ch.Name, Type: ch.Type,
				Hash: t.m.hashes[channelHashKey(id)]})
		}
	}
	return out, nil
}

func (t memReconcileTx) ManagedRules(context.Context) ([]ManagedRule, error) {
	var out []ManagedRule
	for _, r := range t.m.rules {
		if r.ManagedBy == ManagedByConfig {
			out = append(out, ManagedRule{ID: r.ID, ChannelID: r.ChannelID, Event: r.Event,
				MinSeverity: r.MinSeverity, Hash: t.m.hashes[ruleHashKey(r.ID)]})
		}
	}
	return out, nil
}

func (t memReconcileTx) InsertChannel(_ context.Context, ch ManagedChannel) (int, error) {
	if err := t.fail(ch.Name); err != nil {
		return 0, err
	}
	for _, cur := range t.m.channels {
		if cur.Name == ch.Name {
			return 0, fmt.Errorf("channel name %q is taken", ch.Name)
		}
	}
	t.m.channelSeq++
	id := t.m.channelSeq
	t.m.channels[id] = Channel{ID: id, Name: ch.Name, Type: ch.Type,
		Config: copyConfig(ch.Config), Enabled: true, ManagedBy: ManagedByConfig}
	t.m.hashes[channelHashKey(id)] = ch.Hash
	return id, nil
}

func (t memReconcileTx) UpdateChannel(_ context.Context, ch ManagedChannel) error {
	if err := t.fail(ch.Name); err != nil {
		return err
	}
	cur, ok := t.m.channels[ch.ID]
	if !ok || cur.ManagedBy != ManagedByConfig {
		return fmt.Errorf("config channel %d not found", ch.ID)
	}
	cur.Type, cur.Config, cur.Enabled = ch.Type, copyConfig(ch.Config), true
	t.m.channels[ch.ID] = cur
	t.m.hashes[channelHashKey(ch.ID)] = ch.Hash
	return nil
}

func (t memReconcileTx) DeleteChannel(_ context.Context, ch ManagedChannel) error {
	if err := t.fail(ch.Name); err != nil {
		return err
	}
	delete(t.m.channels, ch.ID)
	delete(t.m.hashes, channelHashKey(ch.ID))
	return nil
}

func (t memReconcileTx) InsertRule(_ context.Context, r ManagedRule) (int, error) {
	if err := t.fail(t.m.channels[r.ChannelID].Name); err != nil {
		return 0, err
	}
	t.m.ruleSeq++
	t.m.rules = append(t.m.rules, Rule{ID: t.m.ruleSeq, ChannelID: r.ChannelID,
		Event: r.Event, MinSeverity: r.MinSeverity, Enabled: true,
		ManagedBy: ManagedByConfig})
	t.m.hashes[ruleHashKey(t.m.ruleSeq)] = r.Hash
	return t.m.ruleSeq, nil
}

func (t memReconcileTx) DeleteRule(_ context.Context, r ManagedRule) error {
	kept := t.m.rules[:0]
	for _, cur := range t.m.rules {
		if cur.ID != r.ID {
			kept = append(kept, cur)
		}
	}
	t.m.rules = kept
	delete(t.m.hashes, ruleHashKey(r.ID))
	return nil
}
