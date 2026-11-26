package mcpserver

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ring is the bounded in-memory buffer of an emit-only event type. Its
// cursors read "e<epoch>.<n>", n the sequence number of the newest event
// the cursor has passed. It knows nothing of how events travel: poll
// reads it on request, and a consumer that is to be woken as events
// arrive, as a stream would be, reads it the same way after a signal
// from [ring.watch].
type ring struct {
	epoch     string
	now       func() time.Time
	maxAge    time.Duration
	maxEvents int

	mu      sync.Mutex
	entries []ringEntry
	// last is the sequence number of the newest event, 0 before any.
	last uint64
	// evicted is the sequence number of the newest event dropped, so a
	// cursor below it has a gap after it and one at it has none.
	evicted  uint64
	watchers map[int]func()
	watchSeq int
}

type ringEntry struct {
	seq uint64
	at  time.Time
	ev  eventRecord
}

func newRing(epoch string, now func() time.Time, b EmitBuffer) *ring {
	return &ring{epoch: epoch, now: now, maxAge: b.MaxAge, maxEvents: b.MaxEvents, watchers: map[int]func(){}}
}

// continueFrom has r carry on the sequence of old, with none of its
// events, so a cursor from old reads as a gap and not as a position in
// the new events. Old's epoch is kept for the same reason: cursors that
// were valid stay parseable.
func (r *ring) continueFrom(old *ring) {
	old.mu.Lock()
	defer old.mu.Unlock()
	r.epoch = old.epoch
	r.last = old.last
	r.evicted = old.last
}

func (r *ring) format(seq uint64) string { return fmt.Sprintf("e%s.%d", r.epoch, seq) }

// parse reads a cursor of this ring.
func (r *ring) parse(c string) (uint64, bool) {
	rest, ok := strings.CutPrefix(c, "e")
	if !ok {
		return 0, false
	}
	epoch, n, ok := strings.Cut(rest, ".")
	if !ok || epoch != r.epoch {
		return 0, false
	}
	seq, err := strconv.ParseUint(n, 10, 64)
	return seq, err == nil
}

// append adds ev as the newest event, and wakes the watchers.
func (r *ring) append(ev eventRecord) {
	r.mu.Lock()
	r.last++
	r.entries = append(r.entries, ringEntry{seq: r.last, at: r.now(), ev: ev})
	r.evictLocked()
	ws := make([]func(), 0, len(r.watchers))
	for _, w := range r.watchers {
		ws = append(ws, w)
	}
	r.mu.Unlock()
	for _, w := range ws {
		w()
	}
}

func (r *ring) evictLocked() {
	n := 0
	floor := r.now().Add(-r.maxAge)
	for n < len(r.entries) && (len(r.entries)-n > r.maxEvents || r.entries[n].at.Before(floor)) {
		r.evicted = r.entries[n].seq
		n++
	}
	if n > 0 {
		// A new slice, not a reslice, so the entries a reader has not
		// finished with are not kept alive by the buffer's head forever.
		r.entries = append([]ringEntry(nil), r.entries[n:]...)
	}
}

// watch has fn called, with no lock held, after each event is appended.
// fn must not block. The cancel func stops it.
func (r *ring) watch(fn func()) (cancel func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.watchSeq++
	id := r.watchSeq
	r.watchers[id] = fn
	return func() {
		r.mu.Lock()
		delete(r.watchers, id)
		r.mu.Unlock()
	}
}

// ringRead is what one read of the ring found.
type ringRead struct {
	events []Event
	// seqs are the sequence numbers of events, which a consumer that
	// acknowledges them one by one keeps its position by.
	seqs      []uint64
	cursor    string
	truncated bool
	hasMore   bool
}

// read returns the events after cursor, which is nil for now. floor, when
// not zero, skips events older than it and says truncated if it did. sel
// maps an event to what a subscription is to see, or false to skip it;
// the cursor passes the skipped ones. At most max events are returned,
// with hasMore when others are ready.
func (r *ring) read(cursor *string, max int, floor time.Time, sel func(eventRecord) (Event, bool)) ringRead {
	r.mu.Lock()
	r.evictLocked()
	snap, last, evicted := r.entries, r.last, r.evicted
	r.mu.Unlock()

	if cursor == nil {
		return ringRead{cursor: r.format(last)}
	}
	pos, ok := r.parse(*cursor)
	if !ok || pos > last || pos < evicted {
		// A gap: another process's cursor, or one the buffer has moved
		// past. Start at the oldest event still held.
		return ringRead{cursor: r.format(evicted), truncated: true, hasMore: last > evicted}
	}
	out := ringRead{}
	for _, en := range snap {
		if en.seq <= pos {
			continue
		}
		if !floor.IsZero() && en.at.Before(floor) {
			out.truncated = true
			pos = en.seq
			continue
		}
		if len(out.events) >= max {
			break
		}
		pos = en.seq
		if ev, ok := sel(en.ev); ok {
			out.events = append(out.events, ev)
			out.seqs = append(out.seqs, en.seq)
		}
	}
	out.cursor = r.format(pos)
	out.hasMore = pos < last
	return out
}
