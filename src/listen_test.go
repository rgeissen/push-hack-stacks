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
	name, root, bass, inv, known := parseChordOf("Cmaj7|0|0|0|1")
	if name != "Cmaj7" || root != 0 || bass != 0 || inv != 0 || !known {
		t.Errorf("Cmaj7: %q %d %d %d %v", name, root, bass, inv, known)
	}
	if name, _, bass, inv, _ := parseChordOf("C|0|1|4|1"); name != "C" || bass != 4 || inv != 1 {
		t.Errorf("C/E: %q bass %d inv %d", name, bass, inv)
	}
	if name, root, _, _, _ := parseChordOf(""); name != "" || root != -1 {
		t.Errorf("empty: %q %d", name, root)
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
		name, _, _, _, known := parseChordOf(reply)
		if !ok || name != want || !known {
			t.Errorf("%s: %q (%q), want %s", held, name, reply, want)
		}
	}
}
