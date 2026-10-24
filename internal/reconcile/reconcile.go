// Package reconcile clamps a desired runner count into a configured
// capacity window.
package reconcile

// Clamp bounds v to [lo, hi]. The caller is expected to pass lo <= hi;
// if lo > hi the range is empty and hi is returned.
func Clamp(v, lo, hi int) int {
	if lo > hi {
		return hi
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
