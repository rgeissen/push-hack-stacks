// Package dsp hosts the vendored Stacks C core through its plugin_api_v2
// tool entry, the same entry Schwung uses on Move.
//
// NOT THREAD-SAFE, BY DESIGN. Stacks' state machine expects one caller:
// every Instance method, and New/Destroy, must run on the same goroutine
// (the engine, which locks its OS thread). Its own worker thread is
// Stacks' business; it never calls back into Go.
package dsp

/*
#cgo CFLAGS: -O2 -D_GNU_SOURCE -I${SRCDIR}/../upstream/stacks/src
#cgo LDFLAGS: -lm -lpthread
#include <stdlib.h>
#include "stacks_push.h"
*/
import "C"

import (
	"errors"
	"unsafe"
)

const (
	SampleRate     = 44100
	FramesPerBlock = 128 // Move's block; render_block's native size

	// SourceExternal tags MIDI that came from outside (Live's clock, a
	// keyboard routed to Stacks In). Internal (0) means Move pad ids,
	// which Stacks drops -- Push never sends those.
	SourceExternal = 2

	stateMax = 32768 // get_param("state") cap is 16384 in stacks.c
)

// Hooks are what the C core calls back into. Set them before Init; they
// run on the engine goroutine, inside an Instance call.
var Hooks struct {
	Log   func(msg string)
	Notes func(msg []byte) // one raw MIDI message, e.g. 90 3C 64
	Tempo func() float32
}

// Init binds the host API and fetches the tool entry. Call once.
func Init() error {
	if C.sp_init(SampleRate, FramesPerBlock) == 0 {
		return errors.New("stacks: move_plugin_init_v2 returned no v2 API")
	}
	return nil
}

// Instance is one Stacks state machine.
type Instance struct {
	p   unsafe.Pointer
	buf []byte
	pcm []int16
}

// New creates an instance. configJSON may carry {"state_path":"..."}.
func New(moduleDir, configJSON string) (*Instance, error) {
	dir, cfg := C.CString(moduleDir), C.CString(configJSON)
	defer C.free(unsafe.Pointer(dir))
	defer C.free(unsafe.Pointer(cfg))
	p := C.sp_create(dir, cfg)
	if p == nil {
		return nil, errors.New("stacks: create_instance failed")
	}
	return &Instance{p: p, buf: make([]byte, stateMax), pcm: make([]int16, FramesPerBlock*2)}, nil
}

// Destroy saves state (Stacks writes it synchronously) and frees the instance.
func (i *Instance) Destroy() {
	if i.p != nil {
		C.sp_destroy(i.p)
		i.p = nil
	}
}

// OnMIDI feeds one raw MIDI message (clock, start, stop, a note).
func (i *Instance) OnMIDI(msg []byte, source int) {
	if len(msg) == 0 {
		return
	}
	C.sp_on_midi(i.p, (*C.uint8_t)(unsafe.Pointer(&msg[0])), C.int(len(msg)), C.int(source))
}

// SetParam writes one parameter, exactly as a surface would.
func (i *Instance) SetParam(key, val string) {
	k, v := C.CString(key), C.CString(val)
	defer C.free(unsafe.Pointer(k))
	defer C.free(unsafe.Pointer(v))
	C.sp_set_param(i.p, k, v)
}

// GetParam reads one parameter. ok is false when Stacks does not know the key.
func (i *Instance) GetParam(key string) (string, bool) {
	k := C.CString(key)
	defer C.free(unsafe.Pointer(k))
	n := int(C.sp_get_param(i.p, k, (*C.char)(unsafe.Pointer(&i.buf[0])), C.int(len(i.buf))))
	if n < 0 {
		return "", false
	}
	if n >= len(i.buf) {
		n = len(i.buf) - 1 // snprintf reports what it wanted, not what fit
	}
	return string(i.buf[:n]), true
}

// Render runs one block: Stacks' scheduler advances and emits notes.
func (i *Instance) Render() {
	C.sp_render(i.p, (*C.int16_t)(unsafe.Pointer(&i.pcm[0])), FramesPerBlock)
}

//export goStacksLog
func goStacksLog(msg *C.char) {
	if Hooks.Log != nil {
		Hooks.Log(C.GoString(msg))
	}
}

//export goStacksBPM
func goStacksBPM() C.float {
	if Hooks.Tempo != nil {
		return C.float(Hooks.Tempo())
	}
	return 120
}

// goStacksInject receives a USB-MIDI packet, as Move would: byte 0 is
// cable<<4 | CIN, bytes 1-3 the message. Stacks only injects notes.
//
//export goStacksInject
func goStacksInject(pkt *C.uint8_t, n C.int) C.int {
	if n < 4 || Hooks.Notes == nil {
		return n
	}
	b := unsafe.Slice((*byte)(unsafe.Pointer(pkt)), int(n))
	msg := []byte{b[1], b[2], b[3]}
	if k := b[1] & 0xF0; k == 0xC0 || k == 0xD0 {
		msg = msg[:2]
	}
	Hooks.Notes(msg)
	return n
}
