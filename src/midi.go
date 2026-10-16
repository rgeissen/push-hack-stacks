package main

// midi.go -- Stacks' two ALSA ports and the tempo it hears.
//
//   Stacks Out  readable. Live connects to it by itself (~20 s) and plays
//               the notes on the selected track.
//   Stacks In   writable. Listed as an OUTPUT in Push's MIDI settings; with
//               Sync on, Live sends Start, clock and Stop here. Notes sent
//               here select chords, as a keyboard does on Move.

import (
	"log"
	"time"

	"github.com/federico-pepe/ableton-push-hack/core/alsaseq"
)

const (
	portOut = "Stacks Out"
	portIn  = "Stacks In"
)

func openPort(client, port string, caps uint32) (*alsaseq.Client, error) {
	c, err := alsaseq.Open()
	if err != nil {
		return nil, err
	}
	if err := c.SetName(client); err != nil {
		log.Printf("midi: %v (client keeps its default name)", err)
	}
	if _, err := c.CreatePort(port, caps, alsaseq.PortTypeMidi|alsaseq.PortTypeApp); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// notesOut sends Stacks' notes to whatever is connected to Stacks Out and
// remembers what is sounding, so a shutdown never strands a note.
// Engine goroutine only.
type notesOut struct {
	c        *alsaseq.Client
	sounding map[[2]byte]bool // {channel, note}
}

func newNotesOut(c *alsaseq.Client) *notesOut {
	return &notesOut{c: c, sounding: map[[2]byte]bool{}}
}

func (o *notesOut) send(msg []byte) {
	if len(msg) == 3 {
		key := [2]byte{msg[0] & 0x0F, msg[1]}
		switch {
		case msg[0]&0xF0 == 0x90 && msg[2] > 0:
			o.sounding[key] = true
		case msg[0]&0xF0 == 0x80, msg[0]&0xF0 == 0x90:
			delete(o.sounding, key)
		}
	}
	if _, err := o.c.SendMIDI(alsaseq.Subscribers, msg); err != nil {
		log.Printf("midi out: %v", err)
	}
}

func (o *notesOut) allOff() {
	for k := range o.sounding {
		o.c.SendMIDI(alsaseq.Subscribers, []byte{0x80 | k[0], k[1], 0})
	}
	o.sounding = map[[2]byte]bool{}
}

// inHandler turns sequencer events from Stacks In into raw MIDI for the
// engine. Announce events (port subscribed and so on) have no MIDI form
// and are dropped by DecodeMIDI.
type inHandler struct{ out chan<- midiIn }

func (h inHandler) Fixed(evType uint8, _ alsaseq.Addr, data []byte) {
	if evType == alsaseq.EvSensing {
		return
	}
	if msg := alsaseq.DecodeMIDI(evType, data); msg != nil {
		h.out <- midiIn{msg: msg, at: time.Now()}
	}
}

func (inHandler) VarLen(uint8, alsaseq.Addr, []byte) {}

// tempo estimates Live's BPM from its clock: 24 pulses per beat, averaged
// over the last beat. It keeps the last estimate when the clock stops, and
// says 120 until it has heard one. Engine goroutine only.
type tempo struct {
	last   time.Time
	iv     [24]time.Duration
	n, i   int
	sum    time.Duration
	cached float32
}

func (t *tempo) feed(m midiIn) {
	switch m.msg[0] {
	case 0xFA, 0xFC:
		t.last = time.Time{} // a stop or restart breaks the pulse chain
	case 0xF8:
		if !t.last.IsZero() {
			d := m.at.Sub(t.last)
			// 20..300 BPM is 125..8.3 ms per pulse; anything else is a gap.
			if d >= 8*time.Millisecond && d <= 125*time.Millisecond {
				if t.n == len(t.iv) {
					t.sum -= t.iv[t.i]
				} else {
					t.n++
				}
				t.iv[t.i] = d
				t.sum += d
				t.i = (t.i + 1) % len(t.iv)
				if t.n >= 6 {
					t.cached = float32(time.Minute) / float32(24*t.sum/time.Duration(t.n))
				}
			}
		}
		t.last = m.at
	}
}

func (t *tempo) bpm() float32 {
	if t.cached <= 0 {
		return 120
	}
	return t.cached
}
