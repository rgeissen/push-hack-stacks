package main

import "testing"

// Every push-manager client names Stacks. While Stacks owns the surface
// push-manager refuses (409) frames from a client that does not: the
// screen's client did not, and Stacks' screen never showed under Push Hack
// Home.
func TestClientsNameStacks(t *testing.T) {
	scr := newScreen(nil, nil)
	if scr.pm.App != "stacks" {
		t.Fatalf("screen's client names %q", scr.pm.App)
	}
	if s := newSurface(nil, scr, nil); s.pm.App != "stacks" {
		t.Fatalf("surface's client names %q", s.pm.App)
	}
}
