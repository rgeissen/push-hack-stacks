// Command notetest plays chords into a Live MIDI input, note by note, for
// testing listen mode on a device without anyone at the pads: notes into
// the selected track come back through the listen track to Stacks Listen.
// A dev tool; nothing ships it.
//
//	notetest -dst 128:5 -chords "60.64.67 60.64.67.71 52.55.60" -gap 150ms -hold 1500ms
package main

import (
	"flag"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/federico-pepe/ableton-push-hack/core/alsaseq"
)

func main() {
	dst := flag.String("dst", "", "Live's input port, client:port (see /proc/asound/seq/clients)")
	chords := flag.String("chords", "60.64.67 60.64.67.71", "chords, notes joined by '.', chords by spaces")
	gap := flag.Duration("gap", 150*time.Millisecond, "between one note of a chord and the next")
	hold := flag.Duration("hold", 1500*time.Millisecond, "how long a whole chord is held")
	flag.Parse()

	var to alsaseq.Addr
	if c, p, ok := strings.Cut(*dst, ":"); ok {
		ci, _ := strconv.Atoi(c)
		pi, _ := strconv.Atoi(p)
		to = alsaseq.Addr{Client: byte(ci), Port: byte(pi)}
	} else {
		log.Fatal("-dst client:port is needed")
	}
	c, err := alsaseq.Open()
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()
	c.SetName("Stacks notetest")
	if _, err := c.CreatePort("notetest", alsaseq.CapRead, alsaseq.PortTypeMidi|alsaseq.PortTypeApp); err != nil {
		log.Fatal(err)
	}
	for _, ch := range strings.Fields(*chords) {
		var notes []byte
		for _, n := range strings.Split(ch, ".") {
			v, err := strconv.Atoi(n)
			if err != nil || v < 0 || v > 127 {
				log.Fatalf("bad note %q", n)
			}
			notes = append(notes, byte(v))
		}
		for _, n := range notes {
			if _, err := c.SendMIDI(to, []byte{0x90, n, 100}); err != nil {
				log.Fatal(err)
			}
			log.Printf("on %d", n)
			time.Sleep(*gap)
		}
		time.Sleep(*hold)
		for _, n := range notes {
			c.SendMIDI(to, []byte{0x80, n, 0})
		}
		log.Printf("off %s", ch)
		time.Sleep(400 * time.Millisecond)
	}
}
