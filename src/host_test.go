package main

import (
	"strings"
	"testing"
	"time"
)

func TestTempoFromClock(t *testing.T) {
	var tp tempo
	if tp.bpm() != 120 {
		t.Fatalf("before any clock: %v, want 120", tp.bpm())
	}
	at := time.Unix(0, 0)
	tp.feed(midiIn{msg: []byte{0xFA}, at: at})
	pulse := time.Minute / (24 * 98) // 98 BPM
	for i := 0; i < 48; i++ {
		tp.feed(midiIn{msg: []byte{0xF8}, at: at})
		at = at.Add(pulse)
	}
	if got := tp.bpm(); got < 97.9 || got > 98.1 {
		t.Errorf("bpm = %v, want 98", got)
	}
	// A long gap (transport stopped, then restarted) is not a pulse.
	at = at.Add(3 * time.Second)
	tp.feed(midiIn{msg: []byte{0xF8}, at: at})
	if got := tp.bpm(); got < 97.9 || got > 98.1 {
		t.Errorf("after a gap bpm = %v, want 98 kept", got)
	}
}

func TestShimRunsBeforePanel(t *testing.T) {
	p := panelPage()
	shim := strings.Index(p, "window.schwungRemote = {")
	panel := strings.Index(p, "var R = window.schwungRemote")
	if shim < 0 || panel < 0 || shim > panel {
		t.Fatalf("shim at %d, panel bridge at %d: the shim must come first", shim, panel)
	}
	if !strings.Contains(p, `component: "tool"`) {
		t.Error("the panel only takes the Tool-tab path when component is \"tool\"")
	}
}
