// Package dispatch is the single sanctioned bridge from background goroutines
// back onto the GTK main loop. GTK4 is single-threaded: every widget mutation
// must happen on the thread running the application, so any goroutine that needs
// to touch the UI marshals a closure through Main. Worker code does its network
// and database work off-thread and passes only the cheap widget update here.
package dispatch

import "github.com/diamondburned/gotk4/pkg/core/glib"

// Main schedules fn to run once on the GTK main loop. It is safe to call from
// any goroutine. Do not block inside fn (no network or long-running SQL) — do
// that work in the goroutine and marshal only the resulting UI update.
func Main(fn func()) {
	glib.IdleAdd(fn)
}
