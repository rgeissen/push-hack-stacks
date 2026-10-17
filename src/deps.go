package main

// deps.go -- what Stacks needs of the framework to own the Push surface,
// watched and said plainly (CLAUDE.md, "Display-owning hacks").
//
// Playing into Live and the browser panel need none of it: those are MIDI
// ports and Stacks' own HTTP server. Taking the screen, pads and buttons
// needs push-manager (the display and the MIDI intercept), push-display's
// framebuffer, and a release of both that knows pass-through buttons and
// raw frames. Without the last, Play would be swallowed by the filter and
// every frame would be a 404 -- so Stacks refuses to take the surface and
// says what to update, instead of half-working.

import (
	"log"
	"sync"
	"time"

	"github.com/federico-pepe/ableton-push-hack/core/pmclient"
)

const depsEvery = 10 * time.Second

type deps struct {
	pm *pmclient.Client

	mu   sync.Mutex
	last string // the last problem logged, "" for ready
	seen bool
}

func newDeps(pm *pmclient.Client) *deps { return &deps{pm: pm} }

// Problem checks now: "" when the surface can be taken, else what to do.
func (d *deps) Problem() string {
	st, err := d.pm.DisplayStatus()
	if err != nil {
		return "push-manager is not reachable on port 7701: install or start Push Hack"
	}
	if !st.Connected {
		return "push-manager is up but push-display's framebuffer is not connected: " +
			"install push-display and restart Push"
	}
	f, err := d.pm.MidiFilterStatus()
	if err != nil {
		return "push-manager did not answer its MIDI filter status: update Push Hack"
	}
	if !f.SurfaceOwning {
		return "this Push Hack is too old for Stacks on the Push surface " +
			"(no pass-through buttons or raw frames): update Push Hack. " +
			"Stacks still plays into Live and its browser panel works"
	}
	return ""
}

// note logs a state only when it changed (and the first one).
func (d *deps) note(problem string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.seen && problem == d.last {
		return
	}
	d.seen, d.last = true, problem
	if problem == "" {
		log.Printf("deps: push-manager and push-display ready; Shift + Help opens Stacks on Push")
	} else {
		log.Printf("deps: %s", problem)
	}
}

// run checks at start and every depsEvery until done.
func (d *deps) run(done <-chan struct{}) {
	t := time.NewTicker(depsEvery)
	defer t.Stop()
	for {
		d.note(d.Problem())
		select {
		case <-done:
			return
		case <-t.C:
		}
	}
}
