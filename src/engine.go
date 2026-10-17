package main

// engine.go -- the ONE goroutine that owns the Stacks instance.
//
// Every call into the C core happens here, on a locked OS thread: blocks on
// schedule, MIDI the moment it lands, and surface reads and writes queued
// as closures. A DSP's state is not thread-safe, and a second caller was the
// silent-race bug in push-hack's first DSP host (docs/push3-dsp-hosting.md).

import (
	"context"
	"encoding/json"
	"log"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/rgeissen/push-hack-stacks/dsp"
)

const (
	blockDur  = time.Second * dsp.FramesPerBlock / dsp.SampleRate // ~2.9 ms
	pollEvery = 100 * time.Millisecond
	// A stall longer than this is skipped, not caught up: rendering 100 ms
	// of blocks back to back fires every due note in one burst.
	maxCatchUp = 50 * time.Millisecond
	playPush   = 250 * time.Millisecond // rui_play cadence while running

	// A transport Live started but stopped clocking is released after
	// this: a Start with no clock (Live's audio engine not running), or a
	// clock that dies without a Stop (Live quit, a USB reset), otherwise
	// leaves Stacks waiting on a downbeat that never comes -- stuck on its
	// first chord. 1.5 s is longer than one clock at any tempo Live runs.
	clockTimeout = 1500 * time.Millisecond
)

type midiIn struct {
	msg []byte
	at  time.Time
}

// Engine runs the instance and publishes what surfaces need.
type Engine struct {
	midi  chan midiIn
	calls chan func(*dsp.Instance)
	tempo tempo

	publish func(map[string]string) // called on the engine goroutine

	mu     sync.Mutex
	params map[string]string // latest full map, for a fresh surface
	ready  bool

	lastRev  string
	lastPlay string
	lastPush time.Time

	xport     bool      // Live's transport, as its Start/Stop said
	lastPulse time.Time // the last clock, or the Start when none yet

	stats timing
}

func newEngine(publish func(map[string]string)) *Engine {
	return &Engine{
		midi:    make(chan midiIn, 1024),
		calls:   make(chan func(*dsp.Instance), 64),
		publish: publish,
		params:  map[string]string{},
	}
}

// Run creates the instance and drives it until ctx ends, then saves.
func (e *Engine) Run(ctx context.Context, moduleDir, config string, out *notesOut) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	dsp.Hooks.Log = func(m string) { log.Print(strings.TrimRight(m, "\n")) }
	dsp.Hooks.Tempo = e.tempo.bpm
	dsp.Hooks.Notes = out.send
	if err := dsp.Init(); err != nil {
		return err
	}
	inst, err := dsp.New(moduleDir, config)
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.ready = true
	e.mu.Unlock()
	defer func() {
		// Unpark before the save, or a closed Stacks saves Run as the park
		// left it (off) and the next start has lost the user's Run.
		inst.SetParam("parked", "off")
		inst.Destroy() // Stacks saves state synchronously here
		out.allOff()
	}()
	// Stacks starts with the surface closed: parked, as SetActive(false)
	// leaves it. Applied before the first block, so nothing plays first.
	inst.SetParam("parked", "on")

	next := time.Now()
	nextPoll := next
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case m := <-e.midi:
			e.stats.heard(m.msg)
			e.tempo.feed(m)
			switch m.msg[0] {
			case 0xFA, 0xFB:
				e.xport, e.lastPulse = true, m.at
			case 0xFC:
				e.xport = false
			case 0xF8:
				e.lastPulse = m.at
			}
			inst.OnMIDI(m.msg, dsp.SourceExternal)
		case f := <-e.calls:
			f(inst)
		case <-timer.C:
			now := time.Now()
			e.stats.wake(now.Sub(next))
			if now.Sub(next) > maxCatchUp {
				e.stats.skipped++
				next = now
			}
			for !next.After(now) {
				inst.Render()
				next = next.Add(blockDur)
			}
			if e.xport && now.Sub(e.lastPulse) > clockTimeout {
				log.Printf("transport: no clock from Live for %v, releasing", clockTimeout)
				e.xport = false
				inst.OnMIDI([]byte{0xFC}, dsp.SourceExternal)
			}
			if !now.Before(nextPoll) {
				e.poll(inst, now)
				nextPoll = now.Add(pollEvery)
			}
			e.stats.maybeLog(now)
			timer.Reset(time.Until(next))
		}
	}
}

// poll is the Tool tab's loop: rui_poll is the cheap digest
// (rev:on:pos:bpm[:devms]); `state` is read only when rev moves.
func (e *Engine) poll(inst *dsp.Instance, now time.Time) {
	rui, ok := inst.GetParam("rui_poll")
	if !ok {
		return
	}
	rev, play, _ := strings.Cut(rui, ":")
	changed := map[string]string{}
	if rev != e.lastRev {
		e.lastRev = rev
		if s, ok := inst.GetParam("state"); ok {
			var m map[string]string
			if err := json.Unmarshal([]byte(s), &m); err != nil {
				log.Printf("stacks: state is not a flat string map: %v", err)
			}
			for k, v := range m {
				changed[k] = v
			}
		}
	}
	running := strings.HasPrefix(play, "1:")
	if play != e.lastPlay && (!running || now.Sub(e.lastPush) >= playPush ||
		!strings.HasPrefix(e.lastPlay, "1:")) {
		changed["rui_play"] = play
		e.lastPlay = play
		e.lastPush = now
	}
	if len(changed) == 0 {
		return
	}
	e.mu.Lock()
	for k, v := range changed {
		e.params[k] = v
	}
	e.mu.Unlock()
	e.publish(changed)
}

// Snapshot is the full map a newly connected surface starts from.
func (e *Engine) Snapshot() (map[string]string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := make(map[string]string, len(e.params))
	for k, v := range e.params {
		m[k] = v
	}
	return m, e.ready
}

// Call runs f on the engine goroutine and waits, up to timeout.
func (e *Engine) Call(f func(*dsp.Instance), timeout time.Duration) bool {
	done := make(chan struct{})
	select {
	case e.calls <- func(i *dsp.Instance) { f(i); close(done) }:
	case <-time.After(timeout):
		return false
	}
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// timing records how late the block timer wakes, so jitter is measured on
// the device rather than assumed (push-catalog starts hacks at nice 19).
type timing struct {
	n, skipped int
	sum, max   time.Duration
	nextLog    time.Time
	in         map[string]int // MIDI heard on Stacks In, by kind
}

// heard counts what Live sent, so "the transport does nothing" can be told
// apart from "Live sends nothing" by reading the log.
func (t *timing) heard(msg []byte) {
	if t.in == nil {
		t.in = map[string]int{}
	}
	var kind string
	switch {
	case msg[0] == 0xF8:
		kind = "clock"
	case msg[0] == 0xFA:
		kind = "start"
	case msg[0] == 0xFB:
		kind = "continue"
	case msg[0] == 0xFC:
		kind = "stop"
	case msg[0]&0xF0 == 0x90 || msg[0]&0xF0 == 0x80:
		kind = "note"
	default:
		kind = "other"
	}
	t.in[kind]++
	if kind == "start" || kind == "stop" || kind == "continue" {
		log.Printf("transport: %s on Stacks In", kind) // rare: say it now
	}
}

func (t *timing) wake(late time.Duration) {
	if late < 0 {
		late = 0
	}
	t.n++
	t.sum += late
	if late > t.max {
		t.max = late
	}
}

func (t *timing) maybeLog(now time.Time) {
	if now.Before(t.nextLog) {
		return
	}
	if t.n > 0 {
		log.Printf("timing: %d wakes, late avg %v max %v, skipped %d",
			t.n, (t.sum / time.Duration(t.n)).Round(time.Microsecond),
			t.max.Round(time.Microsecond), t.skipped)
	}
	if len(t.in) > 0 {
		log.Printf("midi in: %v", t.in)
	}
	*t = timing{nextLog: now.Add(time.Minute)}
}
