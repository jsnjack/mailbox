package dispatch

import "testing"

// TestMainSchedulesWithoutPanic verifies Main can be called off the main loop
// without panicking. The closure is queued on the default GLib main context;
// it will not run here (no loop), but scheduling must be safe — this guards
// against a binding regression in the glib.IdleAdd wrapper.
func TestMainSchedulesWithoutPanic(t *testing.T) {
	done := make(chan struct{}, 1)
	Main(func() { done <- struct{}{} })
}
