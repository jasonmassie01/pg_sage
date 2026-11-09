package logwatch

// entryBuffer is one bounded parsed-log subscription. A blank database
// receives all entries; fleet subscribers use an exact database name.
type entryBuffer struct {
	database string
	entries  []LogEntry
	evicted  int // entries pushed out unread since the last TakeEvicted
}

// EntrySubscriber receives parsed entries from the same FileWatcher that
// owns RCA classification. It never reads the underlying file itself.
type EntrySubscriber struct {
	watcher *FileWatcher
	id      string
}

// SubscribeEntries registers a bounded parsed-entry subscription. Reusing an
// ID keeps its existing backlog so reconnects cannot silently erase events.
func (fw *FileWatcher) SubscribeEntries(
	id string, database string,
) *EntrySubscriber {
	fw.entryMu.Lock()
	defer fw.entryMu.Unlock()
	if fw.entries == nil {
		fw.entries = make(map[string]*entryBuffer)
	}
	if existing := fw.entries[id]; existing != nil {
		existing.database = database
	} else {
		fw.entries[id] = &entryBuffer{database: database}
	}
	return &EntrySubscriber{watcher: fw, id: id}
}

func (fw *FileWatcher) publishEntry(entry LogEntry) {
	fw.entryMu.Lock()
	defer fw.entryMu.Unlock()
	for _, buffer := range fw.entries {
		if buffer.database != "" && buffer.database != entry.Database {
			continue
		}
		buffer.append(entry)
	}
}

// append queues entry; a full buffer evicts its oldest entry and counts it.
func (b *entryBuffer) append(entry LogEntry) {
	if len(b.entries) < maxBufferSize {
		b.entries = append(b.entries, entry)
		return
	}
	copy(b.entries, b.entries[1:])
	b.entries[len(b.entries)-1] = entry
	b.evicted++
}

// Drain returns each queued entry once for this subscriber.
func (s *EntrySubscriber) Drain() []LogEntry {
	if s == nil || s.watcher == nil {
		return nil
	}
	s.watcher.entryMu.Lock()
	defer s.watcher.entryMu.Unlock()
	buffer := s.watcher.entries[s.id]
	if buffer == nil {
		return nil
	}
	entries := buffer.entries
	buffer.entries = nil
	return entries
}

// TakeEvicted returns how many entries a full buffer evicted unread since
// the last call, and starts the count again.
func (s *EntrySubscriber) TakeEvicted() int {
	if s == nil || s.watcher == nil {
		return 0
	}
	s.watcher.entryMu.Lock()
	defer s.watcher.entryMu.Unlock()
	buffer := s.watcher.entries[s.id]
	if buffer == nil {
		return 0
	}
	n := buffer.evicted
	buffer.evicted = 0
	return n
}

// Stop unregisters the subscriber and releases its backlog.
func (s *EntrySubscriber) Stop() {
	if s == nil || s.watcher == nil {
		return
	}
	s.watcher.entryMu.Lock()
	delete(s.watcher.entries, s.id)
	s.watcher.entryMu.Unlock()
}

func (fw *FileWatcher) clearEntryBuffers() {
	fw.entryMu.Lock()
	for id := range fw.entries {
		delete(fw.entries, id)
	}
	fw.entryMu.Unlock()
}
