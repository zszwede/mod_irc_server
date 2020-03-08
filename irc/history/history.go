// Copyright (c) 2018 Shivaram Lingamneni <slingamn@cs.stanford.edu>
// released under the MIT license

package history

import (
	"github.com/zszwede/mod_irc_server/irc/utils"
	"sync"
	"time"
)

type ItemType uint

const (
	uninitializedItem ItemType = iota
	Privmsg
	Notice
	Join
	Part
	Kick
	Quit
	Mode
	Tagmsg
	Nick
)

const (
	initialAutoSize = 32
)

// a Tagmsg that consists entirely of transient tags is not stored
var transientTags = map[string]bool{
	"+draft/typing": true,
	"+typing":       true, // future-proofing
}

// Item represents an event (e.g., a PRIVMSG or a JOIN) and its associated data
type Item struct {
	Type ItemType

	Nick string
	// this is the uncasefolded account name, if there's no account it should be set to "*"
	AccountName string
	// for non-privmsg items, we may stuff some other data in here
	Message utils.SplitMessage
	Tags    map[string]string
	Params  [1]string
	// for a DM, this is the casefolded nickname of the other party (whether this is
	// an incoming or outgoing message). this lets us emulate the "query buffer" functionality
	// required by CHATHISTORY:
	CfCorrespondent string
}

// HasMsgid tests whether a message has the message id `msgid`.
func (item *Item) HasMsgid(msgid string) bool {
	return item.Message.Msgid == msgid
}

func (item *Item) IsStorable() bool {
	switch item.Type {
	case Tagmsg:
		for name := range item.Tags {
			if !transientTags[name] {
				return true
			}
		}
		return false // all tags were blacklisted
	case Privmsg, Notice:
		// don't store CTCP other than ACTION
		return !item.Message.IsRestrictedCTCPMessage()
	default:
		return true
	}
}

type Predicate func(item *Item) (matches bool)

func Reverse(results []Item) {
	for i, j := 0, len(results)-1; i < j; i, j = i+1, j-1 {
		results[i], results[j] = results[j], results[i]
	}
}

// Buffer is a ring buffer holding message/event history for a channel or user
type Buffer struct {
	sync.RWMutex

	// ring buffer, see irc/whowas.go for conventions
	buffer      []Item
	start       int
	end         int
	maximumSize int
	window      time.Duration

	lastDiscarded time.Time

	nowFunc func() time.Time
}

func NewHistoryBuffer(size int, window time.Duration) (result *Buffer) {
	result = new(Buffer)
	result.Initialize(size, window)
	return
}

func (hist *Buffer) Initialize(size int, window time.Duration) {
	hist.buffer = make([]Item, hist.initialSize(size, window))
	hist.start = -1
	hist.end = -1
	hist.window = window
	hist.maximumSize = size
	hist.nowFunc = time.Now
}

// compute the initial size for the buffer, taking into account autoresize
func (hist *Buffer) initialSize(size int, window time.Duration) (result int) {
	result = size
	if window != 0 {
		result = initialAutoSize
		if size < result {
			result = size // min(initialAutoSize, size)
		}
	}
	return
}

// Add adds a history item to the buffer
func (list *Buffer) Add(item Item) {
	if item.Message.Time.IsZero() {
		item.Message.Time = time.Now().UTC()
	}

	list.Lock()
	defer list.Unlock()

	if len(list.buffer) == 0 {
		return
	}

	list.maybeExpand()

	var pos int
	if list.start == -1 { // empty
		pos = 0
		list.start = 0
		list.end = 1 % len(list.buffer)
	} else if list.start != list.end { // partially full
		pos = list.end
		list.end = (list.end + 1) % len(list.buffer)
	} else if list.start == list.end { // full
		pos = list.end
		list.end = (list.end + 1) % len(list.buffer)
		list.start = list.end // advance start as well, overwriting first entry
		// record the timestamp of the overwritten item
		if list.lastDiscarded.Before(list.buffer[pos].Message.Time) {
			list.lastDiscarded = list.buffer[pos].Message.Time
		}
	}

	list.buffer[pos] = item
}

func (list *Buffer) lookup(msgid string) (result Item, found bool) {
	predicate := func(item *Item) bool {
		return item.HasMsgid(msgid)
	}
	results := list.matchInternal(predicate, false, 1)
	if len(results) != 0 {
		return results[0], true
	}
	return
}

// Between returns all history items with a time `after` <= time <= `before`,
// with an indication of whether the results are complete or are missing items
// because some of that period was discarded. A zero value of `before` is considered
// higher than all other times.
func (list *Buffer) betweenHelper(start, end Selector, cutoff time.Time, pred Predicate, limit int) (results []Item, complete bool, err error) {
	var ascending bool

	defer func() {
		if !ascending {
			Reverse(results)
		}
	}()

	list.RLock()
	defer list.RUnlock()

	if len(list.buffer) == 0 {
		return
	}

	after := start.Time
	if start.Msgid != "" {
		item, found := list.lookup(start.Msgid)
		if !found {
			return
		}
		after = item.Message.Time
	}
	before := end.Time
	if end.Msgid != "" {
		item, found := list.lookup(end.Msgid)
		if !found {
			return
		}
		before = item.Message.Time
	}

	after, before, ascending = MinMaxAsc(after, before, cutoff)

	complete = after.Equal(list.lastDiscarded) || after.After(list.lastDiscarded)

	satisfies := func(item *Item) bool {
		return (after.IsZero() || item.Message.Time.After(after)) &&
			(before.IsZero() || item.Message.Time.Before(before)) &&
			(pred == nil || pred(item))
	}

	return list.matchInternal(satisfies, ascending, limit), complete, nil
}

// implements history.Sequence, emulating a single history buffer (for a channel,
// a single user's DMs, or a DM conversation)
type bufferSequence struct {
	list   *Buffer
	pred   Predicate
	cutoff time.Time
}

func (list *Buffer) MakeSequence(correspondent string, cutoff time.Time) Sequence {
	var pred Predicate
	if correspondent != "" {
		pred = func(item *Item) bool {
			return item.CfCorrespondent == correspondent
		}
	}
	return &bufferSequence{
		list:   list,
		pred:   pred,
		cutoff: cutoff,
	}
}

func (seq *bufferSequence) Between(start, end Selector, limit int) (results []Item, complete bool, err error) {
	return seq.list.betweenHelper(start, end, seq.cutoff, seq.pred, limit)
}

func (seq *bufferSequence) Around(start Selector, limit int) (results []Item, err error) {
	return GenericAround(seq, start, limit)
}

// you must be holding the read lock to call this
func (list *Buffer) matchInternal(predicate Predicate, ascending bool, limit int) (results []Item) {
	if list.start == -1 || len(list.buffer) == 0 {
		return
	}

	var pos, stop int
	if ascending {
		pos = list.start
		stop = list.prev(list.end)
	} else {
		pos = list.prev(list.end)
		stop = list.start
	}

	for {
		if predicate(&list.buffer[pos]) {
			results = append(results, list.buffer[pos])
		}
		if pos == stop || (limit != 0 && len(results) == limit) {
			break
		}
		if ascending {
			pos = list.next(pos)
		} else {
			pos = list.prev(pos)
		}
	}

	return
}

// latest returns the items most recently added, up to `limit`. If `limit` is 0,
// it returns all items.
func (list *Buffer) latest(limit int) (results []Item) {
	results, _, _ = list.betweenHelper(Selector{}, Selector{}, time.Time{}, nil, limit)
	return
}

// LastDiscarded returns the latest time of any entry that was evicted
// from the ring buffer.
func (list *Buffer) LastDiscarded() time.Time {
	list.RLock()
	defer list.RUnlock()

	return list.lastDiscarded
}

func (list *Buffer) prev(index int) int {
	switch index {
	case 0:
		return len(list.buffer) - 1
	default:
		return index - 1
	}
}

func (list *Buffer) next(index int) int {
	switch index {
	case len(list.buffer) - 1:
		return 0
	default:
		return index + 1
	}
}

// return n such that v <= n and n == 2**i for some i
func roundUpToPowerOfTwo(v int) int {
	// http://graphics.stanford.edu/~seander/bithacks.html
	v -= 1
	v |= v >> 1
	v |= v >> 2
	v |= v >> 4
	v |= v >> 8
	v |= v >> 16
	return v + 1
}

func (list *Buffer) maybeExpand() {
	if list.window == 0 {
		return // autoresize is disabled
	}

	length := list.length()
	if length < len(list.buffer) {
		return // we have spare capacity already
	}

	if len(list.buffer) == list.maximumSize {
		return // cannot expand any further
	}

	wouldDiscard := list.buffer[list.start].Message.Time
	if list.window < list.nowFunc().Sub(wouldDiscard) {
		return // oldest element is old enough to overwrite
	}

	newSize := roundUpToPowerOfTwo(length + 1)
	if list.maximumSize < newSize {
		newSize = list.maximumSize
	}
	list.resize(newSize)
}

// Resize shrinks or expands the buffer
func (list *Buffer) Resize(maximumSize int, window time.Duration) {
	list.Lock()
	defer list.Unlock()

	if list.maximumSize == maximumSize && list.window == window {
		return // no-op
	}

	list.maximumSize = maximumSize
	list.window = window

	// three cases where we need to preemptively resize:
	// (1) we are not autoresizing
	// (2) the buffer is currently larger than maximumSize and needs to be shrunk
	// (3) the buffer is currently smaller than the recommended initial size
	//     (including the case where it is currently disabled and needs to be enabled)
	// TODO make it possible to shrink the buffer so that it only contains `window`
	if window == 0 || maximumSize < len(list.buffer) {
		list.resize(maximumSize)
	} else {
		initialSize := list.initialSize(maximumSize, window)
		if len(list.buffer) < initialSize {
			list.resize(initialSize)
		}
	}
}

func (list *Buffer) resize(size int) {
	newbuffer := make([]Item, size)

	if list.start == -1 {
		// indices are already correct and nothing needs to be copied
	} else if size == 0 {
		// this is now the empty list
		list.start = -1
		list.end = -1
	} else {
		currentLength := list.length()
		start := list.start
		end := list.end
		// if we're truncating, keep the latest entries, not the earliest
		if size < currentLength {
			start = list.end - size
			if start < 0 {
				start += len(list.buffer)
			}
			// update lastDiscarded for discarded entries
			for i := list.start; i != start; i = (i + 1) % len(list.buffer) {
				if list.lastDiscarded.Before(list.buffer[i].Message.Time) {
					list.lastDiscarded = list.buffer[i].Message.Time
				}
			}
		}
		if start < end {
			copied := copy(newbuffer, list.buffer[start:end])
			list.start = 0
			list.end = copied % size
		} else {
			lenInitial := len(list.buffer) - start
			copied := copy(newbuffer, list.buffer[start:])
			copied += copy(newbuffer[lenInitial:], list.buffer[:end])
			list.start = 0
			list.end = copied % size
		}
	}

	list.buffer = newbuffer
}

func (hist *Buffer) length() int {
	if hist.start == -1 {
		return 0
	} else if hist.start < hist.end {
		return hist.end - hist.start
	} else {
		return len(hist.buffer) - (hist.start - hist.end)
	}
}
