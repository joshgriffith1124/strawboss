// Package loopdetect flags runs of consecutive identical operations — a
// worker repeating the same tool call, a supervisor re-delegating the
// same task. Small local models loop instead of concluding; detection
// turns a slow timeout into a fast failure with advice.
package loopdetect

// Detector counts the current run of identical keys.
type Detector struct {
	threshold int
	lastKey   string
	runLen    int
}

// New returns a detector that triggers when the same key is observed
// threshold times in a row. Thresholds below 2 are clamped to 2.
func New(threshold int) *Detector {
	if threshold < 2 {
		threshold = 2
	}
	return &Detector{threshold: threshold}
}

// Observe feeds the next operation's identity key. It returns the current
// run length and whether the threshold has been reached (true from the
// triggering observation onward, so callers may act exactly once).
func (d *Detector) Observe(key string) (runLen int, triggered bool) {
	if key == d.lastKey {
		d.runLen++
	} else {
		d.lastKey = key
		d.runLen = 1
	}
	return d.runLen, d.runLen >= d.threshold
}
