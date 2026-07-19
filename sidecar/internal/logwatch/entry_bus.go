package logwatch

// entryBuffer is one bounded parsed-log subscription. A blank database
// receives all entries; fleet subscribers use an exact database name.
type entryBuffer struct {
	database string
	entries  []LogEntry
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
		buffer.entries = appendEntryCapped(buffer.entries, entry)
	}
}

func appendEntryCapped(entries []LogEntry, entry LogEntry) []LogEntry {
	if len(entries) < maxBufferSize {
		return append(entries, entry)
	}
	copy(entries, entries[1:])
	entries[len(entries)-1] = entry
	return entries
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
