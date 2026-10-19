package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/federico-pepe/ableton-push-hack/core/alsaseq"
	"github.com/rgeissen/push-hack-stacks/dsp"
)

func TestListenStateTracksNotes(t *testing.T) {
	l := newListenState()
	feed := func(msg ...byte) {
		ev, data, _ := alsaseq.EncodeMIDI(msg)
		l.Fixed(ev, alsaseq.Addr{}, data)
	}
	feed(0x90, 64, 100)
	feed(0x91, 60, 90) // any channel
	feed(0x90, 67, 80)
	_, held, _, _ := l.snapshot(time.Now())
	if held != "60.64.67" {
		t.Fatalf("held %q, want 60.64.67 (sorted)", held)
	}
	feed(0x80, 64, 0)
	feed(0x90, 67, 0) // note-on velocity 0 is a release
	notes, held, _, _ := l.snapshot(time.Now())
	if held != "60" || len(notes) != 3 {
		t.Errorf("after two releases: held %q, %d notes drawn (released ones fade)", held, len(notes))
	}
	notes, _, _, _ = l.snapshot(time.Now().Add(time.Second))
	if len(notes) != 1 {
		t.Errorf("faded releases kept: %d notes", len(notes))
	}
}

func TestParseChordOf(t *testing.T) {
	c := parseChordOf("Cmaj7|0|0|0|1|7")
	if c.name != "Cmaj7" || c.root != 0 || c.kind != 1 || len(c.omit) != 1 || c.omit[0] != 7 {
		t.Errorf("Cmaj7 no 5th: %+v", c)
	}
	if c := parseChordOf("C|0|-1|6|1|-"); c.inv != -1 || c.bass != 6 || c.omit != nil {
		t.Errorf("C/F#: %+v", c)
	}
	if c := parseChordOf("|0|0|0|2|-"); c.kind != 2 || c.name != "" {
		t.Errorf("an interval: %+v", c)
	}
	if c := parseChordOf("C|0|1|4|1"); c.name != "C" || c.inv != 1 { // an older core: five fields
		t.Errorf("five fields: %+v", c)
	}
	if c := parseChordOf(""); c.name != "" || c.root != -1 {
		t.Errorf("empty: %+v", c)
	}
}

// The core names what the listen port hears, through the same binding the
// screen uses.
func TestCoreNamesAPlayedChord(t *testing.T) {
	if err := dsp.Init(); err != nil {
		t.Fatal(err)
	}
	in, err := dsp.New(t.TempDir(), `{"state_path":"`+filepath.Join(t.TempDir(), "s.json")+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Destroy()
	for held, want := range map[string]string{"60.64.67": "C", "60.64.67.71": "Cmaj7", "52.55.60": "C", "57.60.64.67": "Am7"} {
		reply, ok := in.GetParam("chord_of:" + held)
		c := parseChordOf(reply)
		if !ok || c.name != want || c.kind != 1 {
			t.Errorf("%s: %q (%q), want %s", held, c.name, reply, want)
		}
	}
}

// The board and LAST, with a namer standing in for the core.
func TestListenBoardAndLast(t *testing.T) {
	names := map[string]chordOf{
		"60.64.67":    {name: "C", root: 0, bass: 0, kind: 1},
		"60.64.67.71": {name: "Cmaj7", root: 0, bass: 0, kind: 1},
		"57.60.65":    {name: "F", root: 5, bass: 9, inv: 1, kind: 1},
		"60.64":       {kind: 2},
	}
	namer := func(held string) chordOf {
		if c, ok := names[held]; ok {
			return c
		}
		return chordOf{root: -1, bass: -1}
	}
	l := newListenState()
	now := time.Now()
	step := func(held string) {
		now = now.Add(50 * time.Millisecond)
		l.hear(held, now, namer)
	}
	step("60")
	step("60.64")
	step("60.64.67")
	if l.name != "C" || l.last.name != "" {
		t.Fatalf("C played: board %q, last %q", l.name, l.last.name)
	}
	step("60.64.67.71") // the same chord growing: nothing goes to LAST
	if l.name != "Cmaj7" || l.last.name != "" {
		t.Fatalf("Cmaj7 grown from C: board %q, last %q", l.name, l.last.name)
	}
	step("60.64") // lifted, still held: the board keeps the chord
	if l.name != "Cmaj7" {
		t.Fatalf("lifting notes flipped the board to %q", l.name)
	}
	step("") // every pad up: blank at once, the chord is LAST
	if l.name != "" || l.last.name != "Cmaj7" {
		t.Fatalf("released: board %q, last %q", l.name, l.last.name)
	}
	step("57.60.65")
	if l.name != "F" || l.last.name != "Cmaj7" {
		t.Fatalf("F played: board %q, last %q", l.name, l.last.name)
	}
	step("60.64.67") // straight to another chord, F not released: F is LAST
	if l.name != "C" || l.last.name != "F" {
		t.Fatalf("F to C: board %q, last %q", l.name, l.last.name)
	}
	l.clear()
	sc := l.scene(nil, now, 0, 0xAB5)
	if l.name != "" || l.last.name != "" || sc.Last != "" {
		t.Fatalf("after clear: board %q, last %q", l.name, l.last.name)
	}
}
