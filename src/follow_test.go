package main

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/federico-pepe/ableton-push-hack/core/pmclient"
	"github.com/rgeissen/push-hack-stacks/dsp"
	"github.com/rgeissen/push-hack-stacks/surface"
)

// The follower's table is the core's SCALES[]: each Stacks scale, set on a
// real instance, publishes the mask the table has for it.
func TestStacksScalesMatchTheCore(t *testing.T) {
	if err := dsp.Init(); err != nil {
		t.Fatal(err)
	}
	in, err := dsp.New(t.TempDir(), `{"state_path":"`+filepath.Join(t.TempDir(), "s.json")+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Destroy()
	seen := 0
	for mask, name := range stacksScales {
		in.SetParam("scale", name)
		prog, _ := in.GetParam("prog")
		p, ok := surface.ParseProg(prog)
		if !ok {
			t.Fatalf("%s: prog does not parse", name)
		}
		if p.ScaleMask != mask {
			t.Errorf("%s: the core says mask %#03x, the table %#03x", name, p.ScaleMask, mask)
		}
		seen++
	}
	if seen != 13 {
		t.Errorf("%d scales in the table, Stacks has 13", seen)
	}
}

type fakePush struct {
	sc  pmclient.Scale
	err error
}

func (f *fakePush) LiveScale() (pmclient.Scale, error) { return f.sc, f.err }

type mapParams map[string]string

func (m mapParams) Get(k string) string { return m[k] }
func (m mapParams) Set(k, v string)     { m[k] = v }

func TestFollowerBringsStacksToPushsScale(t *testing.T) {
	push := &fakePush{sc: pmclient.Scale{Supported: true, Root: 2, Name: "Dorian", Intervals: []int{0, 2, 3, 5, 7, 9, 10}}}
	ps := mapParams{"key": "A", "scale": "Minor"}
	var said []string
	dir := t.TempDir()
	f := newFollower(push, ps, dir, func(m string) { said = append(said, m) })

	f.Sync() // off: nothing moves
	if ps["key"] != "A" {
		t.Fatal("followed while off")
	}
	f.Set(true, "")
	if ps["key"] != "D" || ps["scale"] != "Dorian" {
		t.Fatalf("on: Stacks is %s %s, want D Dorian", ps["key"], ps["scale"])
	}
	// Push changes key; a scale Stacks lacks keeps Stacks' scale, follows the key.
	push.sc = pmclient.Scale{Supported: true, Root: 7, Name: "Whole Tone", Intervals: []int{0, 2, 4, 6, 8, 10}}
	f.Sync()
	if ps["key"] != "G" || ps["scale"] != "Dorian" {
		t.Errorf("Whole Tone: Stacks is %s %s, want G Dorian", ps["key"], ps["scale"])
	}
	if !strings.Contains(strings.Join(said, "|"), "WHOLE TONE IS NOT IN STACKS") {
		t.Errorf("a missing scale was not said: %q", said)
	}
	// No bridge: Stacks is left alone, and the reason is said once.
	push.err = errors.New("live/scale: dial tcp 127.0.0.1:7704: connect: connection refused")
	n := len(said)
	f.Sync()
	f.Sync()
	if len(said) != n+1 || !strings.Contains(said[n], "BROWSER BRIDGE") {
		t.Errorf("bridge down: said %q", said[n:])
	}
	// The choice survives a restart.
	if g := newFollower(push, ps, dir, nil); !g.On() {
		t.Error("following was not remembered")
	}
	f.Set(false, ": YOU TURNED KEY")
	if g := newFollower(push, ps, dir, nil); g.On() {
		t.Error("off was not remembered")
	}
}

// Every Live scale Stacks has is matched by its notes, whatever Live calls it.
func TestStacksScaleByIntervals(t *testing.T) {
	for _, tc := range []struct {
		iv   string
		want string
	}{
		{"0 2 3 5 7 8 11", "HarmMinor"}, {"0 3 5 7 10", "PentMin"}, {"0 2 4 7 9", "PentMaj"},
		{"0 3 5 6 7 10", "Blues"}, {"0 1 2 3 4 5 6 7 8 9 10 11", "Chromatic"},
	} {
		var iv []int
		for _, f := range strings.Fields(tc.iv) {
			n, _ := strconv.Atoi(f)
			iv = append(iv, n)
		}
		if got, ok := stacksScale(iv); !ok || got != tc.want {
			t.Errorf("%s: %q %v, want %s", tc.iv, got, ok, tc.want)
		}
	}
	if _, ok := stacksScale([]int{0, 1, 4, 5, 7, 8, 10}); ok { // Phrygian Dominant
		t.Error("a scale Stacks lacks matched something")
	}
}

// slowPush answers LiveScale only when told to: the round trip into Live.
type slowPush struct {
	asked, answer chan struct{}
	sc            pmclient.Scale
}

func (s *slowPush) LiveScale() (pmclient.Scale, error) {
	s.asked <- struct{}{}
	<-s.answer
	return s.sc, nil
}

// A turn of Stacks' own Scale while Push's scale is being read wins: the
// read must not land after it. It did, and undid the turn.
func TestFollowerTurnDuringReadWins(t *testing.T) {
	push := &slowPush{asked: make(chan struct{}), answer: make(chan struct{}),
		sc: pmclient.Scale{Supported: true, Root: 0, Name: "Major", Intervals: []int{0, 2, 4, 5, 7, 9, 11}}}
	ps := mapParams{"key": "C", "scale": "Major"}
	f := newFollower(push, ps, t.TempDir(), nil)
	f.on = true
	done := make(chan struct{})
	go func() { f.Sync(); close(done) }()
	<-push.asked
	f.Set(false, ": YOU TURNED SCALE") // the turn: following off, then the new value
	ps.Set("scale", "Minor")
	close(push.answer)
	<-done
	if ps["scale"] != "Minor" {
		t.Fatalf("the read in flight undid the turn: scale %s", ps["scale"])
	}
}
