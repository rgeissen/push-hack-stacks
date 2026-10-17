package main

import "testing"

func TestEncoderSteps(t *testing.T) {
	turn := func(span int, deltas ...int) int {
		acc, n := 0, 0
		for _, d := range deltas {
			n += encoderSteps(&acc, d, span)
		}
		return n
	}
	ones := func(k, d int) []int {
		v := make([]int, k)
		for i := range v {
			v[i] = d
		}
		return v
	}
	// Short list (Inversion, 7): six slow ticks per step, either way.
	if n := turn(7, ones(5, 1)...); n != 0 {
		t.Errorf("5 ticks moved inversion %d", n)
	}
	if n := turn(7, ones(6, 1)...); n != 1 {
		t.Errorf("6 ticks moved inversion %d, want 1", n)
	}
	if n := turn(7, ones(12, -1)...); n != -2 {
		t.Errorf("12 ticks down moved inversion %d, want -2", n)
	}
	// A fast flick (Push accelerates: one message worth 9 ticks) moves a
	// short list at most one step per three messages, never a jump.
	if n := turn(7, 9, 9, 9); n != 1 {
		t.Errorf("three accelerated messages moved inversion %d, want 1", n)
	}
	// Medium list (Rhythm, 35): three ticks per step.
	if n := turn(35, ones(9, 1)...); n != 3 {
		t.Errorf("9 ticks moved rhythm %d, want 3", n)
	}
	// Long list (Progression, 143): acceleration kept, two ticks per step.
	if n := turn(143, 20); n != 10 {
		t.Errorf("an accelerated message on a long list moved %d, want 10", n)
	}
	// Turning back cancels a partial step instead of firing it.
	if n := turn(7, 1, 1, 1, -1, -1, -1); n != 0 {
		t.Errorf("there and back moved %d", n)
	}
}
