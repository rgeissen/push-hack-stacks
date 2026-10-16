// Command stacks hosts the Stacks chord-progression tool on Push 3.
//
// The C core is Stacks' own (upstream/stacks, pinned); this binary is the
// host Schwung is on Move: it feeds MIDI in, runs the blocks, sends the
// notes into Live, and serves the browser panel.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/federico-pepe/ableton-push-hack/core/alsaseq"
	"github.com/federico-pepe/ableton-push-hack/core/hackcfg"
	"github.com/federico-pepe/ableton-push-hack/core/httpx"
	"github.com/federico-pepe/ableton-push-hack/core/sse"
)

const defaultPort = 7711

func main() {
	cfgPath := flag.String("config", "hack.json", "path to hack.json")
	dataDir := flag.String("data", "", "state folder (default: <push-hack>/data/stacks, outside the hack folder so a reinstall keeps it)")
	flag.Parse()
	log.SetFlags(log.LstdFlags)

	cfg, err := hackcfg.Load(*cfgPath, defaultPort)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	hackDir, _ := filepath.Abs(filepath.Dir(*cfgPath))
	if *dataDir == "" {
		// <push-hack>/hacks/stacks -> <push-hack>/data/stacks
		*dataDir = filepath.Join(filepath.Dir(filepath.Dir(hackDir)), "data", "stacks")
	}
	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Fatalf("data dir: %v", err)
	}
	log.Printf("stacks %s: port %d, state in %s", cfg.Version, cfg.Port, *dataDir)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	broker := sse.NewBroker[map[string]string](32, true)
	eng := newEngine(broker.Broadcast)

	// HTTP first: the panel answers during the boot-settle wait.
	srv := httpx.NewServer(fmt.Sprintf(":%d", cfg.Port), routes(eng, broker))
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	// No /dev/snd access before the USB-A enumeration window has passed.
	alsaseq.WaitForBootSettle()

	out, err := openPort("Push Hack Stacks", portOut, alsaseq.CapRead|alsaseq.CapSubsRead)
	if err != nil {
		log.Fatalf("midi out: %v", err)
	}
	defer out.Close()
	in, err := openPort("Push Hack Stacks In", portIn, alsaseq.CapWrite|alsaseq.CapSubsWrite)
	if err != nil {
		log.Fatalf("midi in: %v", err)
	}
	go func() {
		err := in.ReadLoop(inHandler{out: eng.midi})
		log.Printf("midi in: %v", err)
	}()
	log.Printf("midi: %q at %d:%d, %q at %d:%d", portOut, out.Addr().Client, out.Addr().Port,
		portIn, in.Addr().Client, in.Addr().Port)

	config := fmt.Sprintf(`{"state_path":"%s"}`, filepath.Join(*dataDir, "state.json"))
	if err := eng.Run(ctx, hackDir, config, newNotesOut(out)); err != nil {
		log.Fatalf("engine: %v", err)
	}
	in.Close() // ends ReadLoop
	// An open event stream never finishes on its own; give it a moment.
	shut, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if srv.Shutdown(shut) != nil {
		srv.Close()
	}
	log.Print("stacks: stopped, state saved")
}
