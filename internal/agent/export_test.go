package agent

import "time"

// SetTimingsForTest shortens the agent's timers and returns a function
// restoring them.
func SetTimingsForTest(d time.Duration) (restore func()) {
	ptrs := []*time.Duration{&backendPollInterval, &backendWatchInterval, &pingInterval, &pingTimeout, &minBackoff, &handshakeTimeout, &acceptSettle, &acceptTimeout}
	vals := []time.Duration{d, d, d, d, d, 10 * d, d, 10 * d}
	old := make([]time.Duration, len(ptrs))
	for i, p := range ptrs {
		old[i], *p = *p, vals[i]
	}
	return func() {
		for i, p := range ptrs {
			*p = old[i]
		}
	}
}
