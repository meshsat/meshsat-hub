package oob

// Window is the RFC 6479 style 64-bit sliding replay window per peer (spec
// section 5): accept a counter above the highest seen, or within 64 below
// it when its bit is clear. Counter 0 is never valid.
type Window struct {
	high uint32
	bits uint64
}

// Load restores persisted state.
func (w *Window) Load(high uint32, bits uint64) { w.high, w.bits = high, bits }

// State returns the persisted form.
func (w *Window) State() (uint32, uint64) { return w.high, w.bits }

// Accept reports whether counter is fresh and records it.
func (w *Window) Accept(counter uint32) bool {
	if counter == 0 {
		return false
	}
	if counter > w.high {
		shift := uint64(counter - w.high)
		if shift >= 64 {
			w.bits = 0
		} else {
			w.bits <<= shift
		}
		w.bits |= 1
		w.high = counter
		return true
	}
	diff := uint64(w.high - counter)
	if diff >= 64 {
		return false
	}
	if w.bits&(1<<diff) != 0 {
		return false
	}
	w.bits |= 1 << diff
	return true
}
