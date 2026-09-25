//go:build !linux

package live

import "time"

// defaultBootTime has no portable source off Linux: the zero time turns the
// boot check off, leaving the pid probe as the only death proof.
func defaultBootTime() time.Time { return time.Time{} }
