package main

// harmonysync.go -- Stacks Harmony: what Stacks plays, for Riffs and any
// other follower (Push Hack Harmony: core/harmony, docs/harmony-sync.md in
// ableton-push-hack).
//
// A port of its own, so Live never sees the SysEx. Each chord change is
// announced `harmonyAhead` clocks before it, from the core's own walk
// (sp_seq_at, pinned against what the core arms by TestSeqAtPredictsTheCore);
// both sides count Live's clock from the same Start, so a follower switches
// on the clock the chord does. What the sequence cannot foresee -- a Start,
// a Stop, an edit, a pad held -- is sent as it happens (delta 0).
//
// The sender keeps a timeline: the chord sounding now, and at most one
// announced change with its clock. The core arms a chord in the block AFTER
// the clock, so what it has armed is compared with the timeline only after
// rendering (afterRender), never on the clock itself.
//
// Engine goroutine only: every call reads the core.

import (
	"log"
	"time"

	"github.com/federico-pepe/ableton-push-hack/core/alsaseq"
	"github.com/federico-pepe/ableton-push-hack/core/harmony"
	"github.com/rgeissen/push-hack-stacks/dsp"
	"github.com/rgeissen/push-hack-stacks/surface"
)

const (
	portHarmony = "Stacks Harmony"
	// harmonyAhead: how early a change is announced, in clocks. A quarter
	// of a beat; Stacks' shortest chord is an eighth of a bar (12 clocks).
	harmonyAhead = 6
	aliveEvery   = time.Second
)

// seqReader is what the sender reads of the core (dsp.Instance; tests fake it).
type seqReader interface {
	SeqAt(ahead int) (idx, pulse int)
	Cur() int
}

var _ seqReader = (*dsp.Instance)(nil)

// harmonySender sends Stacks Harmony. send is the port (tests swap it).
type harmonySender struct {
	send func([]byte)

	prog     surface.Prog
	progOK   bool
	running  bool // Live's transport, as Start/Stop said
	armedRun bool // Run on (and not parked): Stacks plays when the transport runs

	nowIdx    int             // the chord sounding, -1 none
	nowH      harmony.Harmony // what followers were told sounds (a rest keeps the one before)
	told      bool            // nowH has been sent
	pendIdx   int             // an announced change, -1 none
	pendAt    int             // ... at this clock
	nextAhead int             // SeqAt(ahead) on the last clock, -2 unknown
	lastSent  time.Time
}

func newHarmonySender(c *alsaseq.Client) *harmonySender {
	h := &harmonySender{nowIdx: -1, pendIdx: -1, nextAhead: -2}
	h.send = func(b []byte) {
		if err := c.SendSysEx(alsaseq.Subscribers, b); err != nil {
			log.Printf("harmony: %v", err)
		}
	}
	return h
}

// of is chord idx as harmony; ok false for a rest (it plays nothing, so the
// chord before it holds) or an index out of range.
func (h *harmonySender) of(idx int) (harmony.Harmony, bool) {
	if !h.progOK || idx < 0 || idx >= len(h.prog.Chords) {
		return harmony.Harmony{}, false
	}
	c := h.prog.Chords[idx]
	if c.Rest() {
		return harmony.Harmony{}, false
	}
	root := surface.RootPC(c.Name) // the root as it sounds (the name), else as written
	if root < 0 {
		root = surface.RootPC(c.Root)
	}
	if root < 0 {
		return harmony.Harmony{}, false
	}
	lo, set := c.Notes[0], 0
	for _, n := range c.Notes {
		lo = min(lo, n)
		set |= 1 << harmony.PC(n)
	}
	return h.flagged(harmony.Harmony{
		Key: h.prog.Key, Scale: h.prog.ScaleMask & 0xFFF,
		Root: root, Chord: harmony.Rotate(set, root), Bass: harmony.PC(lo),
	}), true
}

func (h *harmonySender) flagged(hm harmony.Harmony) harmony.Harmony {
	hm.Playing = h.running && h.armedRun
	hm.Armed = !h.running && h.armedRun
	return hm
}

func same(a, b harmony.Harmony) bool {
	return a.Same(b) && a.Playing == b.Playing && a.Armed == b.Armed
}

func (h *harmonySender) emit(hm harmony.Harmony, delta int) {
	h.send(harmony.Encode(hm, delta))
	h.lastSent = time.Now()
}

// sendNow: chord idx sounds now. A rest leaves the chord before it.
func (h *harmonySender) sendNow(idx int) {
	h.nowIdx = idx
	hm, ok := h.of(idx)
	if !ok {
		return
	}
	if h.told && same(hm, h.nowH) {
		return
	}
	h.emit(hm, 0)
	h.nowH, h.told = hm, true
}

// announce: chord idx starts at clock at (delta clocks from now).
func (h *harmonySender) announce(idx, at, delta int) {
	h.pendIdx, h.pendAt = idx, at
	hm, ok := h.of(idx)
	if !ok || (h.told && same(hm, h.nowH)) {
		return // a rest, or the same chord again: nothing changes for a follower
	}
	h.emit(hm, delta)
}

// silent: Stacks plays nothing now (Run off, parked, no chords); followers
// stop following it until it plays again.
func (h *harmonySender) silent() {
	h.pendIdx = -1
	hm := h.nowH
	if !h.told {
		hm = harmony.Harmony{Key: harmony.None, Root: harmony.None, Bass: harmony.None}
	}
	if h.told && !hm.Live() {
		return
	}
	hm.Playing, hm.Armed = false, false
	h.emit(hm, 0)
	h.nowH, h.told, h.nowIdx = hm, true, -1
}

// midi follows the transport, after the core has had the message.
func (h *harmonySender) midi(inst seqReader, status byte) {
	switch status {
	case 0xFA, 0xFB: // Start (the next clock is pulse 0), Continue
		h.running = true
		h.pendIdx = -1
		if idx, _ := inst.SeqAt(0); idx >= 0 {
			h.sendNow(idx)
			h.nextAhead, _ = inst.SeqAt(harmonyAhead)
		} else {
			h.silent()
			h.nextAhead = -2
		}
	case 0xFC:
		h.running = false
		h.pendIdx, h.nextAhead = -1, -2
		h.firstChord()
	case 0xF8:
		ahead, pulse := inst.SeqAt(harmonyAhead)
		prev := h.nextAhead
		h.nextAhead = ahead
		switch {
		case ahead < 0:
			if h.nowH.Live() || !h.told {
				h.silent()
			}
		case prev == -2:
		case ahead != prev:
			h.announce(ahead, pulse+harmonyAhead, harmonyAhead)
		}
	}
}

// afterRender: the announced change has come; and what the core armed,
// when it is not what the timeline says (a pad held, an audition, a pad
// let go), sounds now.
func (h *harmonySender) afterRender(inst seqReader) {
	if !h.running {
		return
	}
	seq, pulse := inst.SeqAt(0)
	if seq < 0 {
		return
	}
	if h.pendIdx >= 0 && pulse >= h.pendAt {
		h.nowIdx, h.pendIdx = h.pendIdx, -1
		if hm, ok := h.of(h.nowIdx); ok {
			h.nowH, h.told = hm, true
		}
	}
	if cur := inst.Cur(); cur >= 0 && cur != h.nowIdx && cur != h.pendIdx {
		h.sendNow(cur)
	}
}

// firstChord: stopped, followers learn the chord the next Start plays.
func (h *harmonySender) firstChord() {
	if !h.armedRun || !h.progOK {
		h.silent()
		return
	}
	for i := range h.prog.Chords { // a phrase that opens on rests: its first chord
		if _, ok := h.of(i); ok {
			h.sendNow(i)
			return
		}
	}
	h.silent()
}

// poll takes the engine's changed state: a chord edited, the key, Run;
// and tells followers Stacks is still there.
func (h *harmonySender) poll(inst seqReader, changed map[string]string, now time.Time) {
	_, progChanged := changed["prog"]
	if progChanged {
		if p, ok := surface.ParseProg(changed["prog"]); ok {
			h.prog, h.progOK = p, true
		}
	}
	runV, runChanged := changed["run"]
	if runChanged {
		h.armedRun = runV == "on"
	}
	if progChanged || runChanged {
		switch {
		case !h.armedRun:
			h.silent()
		case !h.running:
			h.firstChord()
		default:
			if seq, _ := inst.SeqAt(0); seq >= 0 {
				if cur := inst.Cur(); cur >= 0 {
					seq = cur
				}
				h.told = h.told && h.nowIdx == seq
				h.sendNow(seq) // an edit of the chord that sounds
			}
			if h.pendIdx >= 0 { // and of the one announced
				_, pulse := inst.SeqAt(0)
				if hm, ok := h.of(h.pendIdx); ok && h.pendAt > pulse {
					h.emit(hm, h.pendAt-pulse)
				}
			}
		}
	}
	if now.Sub(h.lastSent) >= aliveEvery {
		h.send(harmony.EncodeType(harmony.TypeAlive))
		h.lastSent = now
	}
}

// bye: Stacks quits.
func (h *harmonySender) bye() { h.send(harmony.EncodeType(harmony.TypeBye)) }
