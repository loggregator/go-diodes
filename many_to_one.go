package diodes

import (
	"log"
	"sync"
	"sync/atomic"
	"unsafe"
)

// ManyToOne diode is optimal for many writers (go-routines B-n) and a single
// reader (go-routine A). It is not thread safe for multiple readers.
type ManyToOne struct {
	writeIndex uint64
	buffer     []unsafe.Pointer
	readIndex  uint64
	alerter    Alerter
	mu         sync.RWMutex
}

// NewManyToOne creates a new diode (ring buffer). The ManyToOne diode
// is optimzed for many writers (on go-routines B-n) and a single reader
// (on go-routine A). The alerter is invoked on the read's go-routine. It is
// called when it notices that the writer go-routine has passed it and wrote
// over data. A nil can be used to ignore alerts.
func NewManyToOne(size int, alerter Alerter) *ManyToOne {
	if alerter == nil {
		alerter = AlertFunc(func(int) {})
	}

	d := &ManyToOne{
		buffer:  make([]unsafe.Pointer, size),
		alerter: alerter,
	}

	// Start write index at the value before 0
	// to allow the first write to use AddUint64
	// and still have a beginning index of 0
	d.writeIndex = ^d.writeIndex
	return d
}

// Set sets the data in the next slot of the ring buffer.
func (d *ManyToOne) Set(data GenericDataType) {
	d.mu.RLock()
	for {
		writeIndex := atomic.AddUint64(&d.writeIndex, 1)
		idx := writeIndex % uint64(len(d.buffer))
		old := atomic.LoadPointer(&d.buffer[idx])

		if old != nil &&
			(*bucket)(old) != nil &&
			(*bucket)(old).seq > writeIndex-uint64(len(d.buffer)) {
			log.Println("Diode set collision: consider using a larger diode")
			continue
		}

		newBucket := &bucket{
			data: data,
			seq:  writeIndex,
		}

		if !atomic.CompareAndSwapPointer(&d.buffer[idx], old, unsafe.Pointer(newBucket)) {
			log.Println("Diode set collision: consider using a larger diode")
			continue
		}

		d.mu.RUnlock()
		return
	}
}

// TryNext will attempt to read from the next slot of the ring buffer.
// If there is not data available, it will return (nil, false).
func (d *ManyToOne) TryNext() (data GenericDataType, ok bool) {
	// Read a value from the ring buffer based on the readIndex.
	idx := d.readIndex % uint64(len(d.buffer))
	result := (*bucket)(atomic.LoadPointer(&d.buffer[idx]))

	// When the result is nil that means the writer has not had the
	// opportunity to write a value into the diode. This value must be ignored
	// and the read head must not increment.
	if result == nil {
		return nil, false
	}

	// When the seq value is greater than the current read index that means a
	// value was read from idx that overwrote the value that was expected to
	// be at this idx. This happens when the writer has lapped the reader. The
	// reader needs to catch up to the writer so it moves its write head to
	// the new seq, effectively dropping the messages that were not read in
	// between the two values.
	//
	// Here is a simulation of this scenario:
	//
	// 1. Both the read and write heads start at 0.
	//    `| nil | nil | nil | nil |` r: 0, w: -1
	// 2. The writer fills the buffer.
	//    `| 0 | 1 | 2 | 3 |` r: 0, w: 3
	// 3. The writer laps the read head.
	//    `| 4 | 5 | 2 | 3 |` r: 0, w: 5
	// 4. The reader reads the first value, expecting a seq of 0 but reads 4,
	//    this forces the reader to fast forward to 5.
	//    `| 4 | 5 | nil | 3 |` r: 3, w: 5
	//
	if result.seq > d.readIndex {
		d.mu.Lock()
		dropped := (d.writeIndex + 1) - d.readIndex - uint64(len(d.buffer))
		d.alerter.Alert(int(dropped))
		d.readIndex = (d.writeIndex + 1) - uint64(len(d.buffer))
		idx = d.readIndex % uint64(len(d.buffer))
		result = (*bucket)(atomic.LoadPointer(&d.buffer[idx]))
		d.mu.Unlock()
	}

	atomic.CompareAndSwapPointer(&d.buffer[idx], nil, unsafe.Pointer(result))

	// Only increment read index if a regular read occurred (where seq was
	// equal to readIndex) or a value was read that caused a fast forward
	// (where seq was greater than readIndex).
	//
	d.readIndex++
	return result.data, true
}
