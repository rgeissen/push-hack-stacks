# Stacks for Push 3

Stacks is a chord-progression tool. It was made for Ableton Move as a
[Schwung](https://github.com/charlesvestal/schwung) module
([schwung-stacks](https://github.com/rgeissen/schwung-stacks)). This repo
runs the same Stacks on Ableton Push 3 Standalone, as a
[Push Hack](https://github.com/federico-pepe/ableton-push-hack) catalog hack.

Status: early. Stacks plays into Live and follows Live's transport. You
edit it on Push itself (screen, encoders, buttons and pads) or from a
browser.

## On Push

Every control is in the
[control matrix](https://rgeissen.github.io/push-hack-stacks/control-matrix.html)
(`docs/control-matrix.html`, built from the code by `make matrix`).

- **Shift + Help** opens Stacks on Push's screen. Shift + Help again, or
  the LIVE tab, closes it. A closed Stacks plays nothing.
- **Bottom two pad rows** are the chords. Press a pad to select its chord.
  Hold it to hear the chord with its rhythm, again and again, for as long
  as you hold it.
- **Middle four pad rows** edit the selected chord. Three scene buttons pick
  the page: **1/4** is WRITE, **1/4t** is VOICE, **1/8** is FEEL. The lit
  one is the page you are on.
- **Hold a scene button** to see on the screen what every pad row on that
  page does. Let go and the screen goes back.
- **Top two pad rows** are the 16 patterns. Tap a pad to go to that
  pattern. Hold it to copy the current pattern into it. On the PATTERNS
  tab, each slot shows its chords, and the bottom buttons are **CLEAR
  SLOT** and **COPY TO SLOT**: press one, then tap the slot. Press it again
  to cancel. Holding **Delete** and tapping a pattern pad also clears it.
- **The note lanes keep their size** while you change voicings (inversion,
  octave, colour, grouping). They zoom out only when notes would leave the
  screen. They zoom back in only when the harmony changes: a new chord, a
  chord added or removed, or a pattern or progression loaded. To keep the
  lanes still, the view moves up or down by whole octaves. The thin bar at
  the right edge shows which part of the register is on screen.
- **Inversion shows AUTO** when Voice Grouping is a "dyn" mode. Then
  Stacks picks each chord's inversion itself, so the chord moves as little
  as possible from the chord before. The first chord, and a chord after a
  rest, keep their own inversion. The value you set is kept and comes back
  if you change grouping.

## What it does

- Plays your progression into Live, on the selected track.
- Follows Live's transport: it starts on Play, follows the tempo, and stops
  on Stop.
- Has the same browser panel as on Move, at `http://push.local:<port>/`.
  Push Manager's menu bar links to it.
- Saves everything (the progression, the 16 patterns, all settings) to
  `/data/push-hack/data/stacks/state.json`. A reinstall keeps it.

## Set up on Push (once)

Stacks opens two MIDI ports. Live finds both by itself.

1. **Stacks Out** is the chords. Live plays notes from it on the selected
   track. Nothing to do.
2. **Stacks In** is Live's clock. In Push's **Settings → MIDI**, find
   **Stacks In** under outputs and turn **Sync** on. Without this, Stacks
   cannot follow Play and Stop.

You can also send notes from a Live track to Stacks In. A note selects a
chord, the same way a keyboard does on Move.

## Requires

- Push 3 Standalone with Push Hack installed: Push Manager (port 7701)
  and Push Display.
- For Stacks on Push's screen, pads and buttons: a Push Hack release with
  pass-through buttons and raw frames
  ([#33](https://github.com/federico-pepe/ableton-push-hack/pull/33)).
  With an older one, Stacks still plays into Live and the browser panel
  works. Shift + Help then shows on the screen what to update. The log
  (`/data/push-hack/logs/stacks.log`) says it too, with lines starting
  `deps:`.
- Not supported yet: Read Clip and Read Track. On Move they read the set
  file, and Live's sets are a different format. They will come through
  Browser Bridge.

## Build and run (developers)

Push is x86-64 Linux with glibc 2.35. Stacks has a C core, so the binary
uses cgo, and it is linked statically so it runs on any glibc. The build
runs in Docker. You do not need Go installed.

```bash
make test                       # Go tests, with the C core, in an amd64 container
make build                      # ./stacks, static linux/amd64
./scripts/deploy.sh push.local  # copy to the Push and start it as push-catalog would
./scripts/deploy.sh push.local stop
```

The log is `/data/push-hack/logs/stacks.log` on the Push. Once a minute it
prints how late the block timer woke, so timing is measured, not assumed.

### How it is built

- `src/upstream/stacks/` is Stacks' own code, copied from schwung-stacks at
  the commit in `UPSTREAM_REF`. Do not edit it here.
  `./scripts/sync-stacks.sh [ref]` copies a new version, and
  `./scripts/sync-stacks.sh --check` checks the copy.
- `src/dsp/` builds that code without changes, and gives it the host API
  that Schwung gives it on Move (`host.c`, `stacks_unit.c`).
- `src/engine.go` is the one goroutine that calls into Stacks. It renders
  a 128-frame block every 2.9 ms, passes MIDI through as it arrives, and
  polls `rui_poll` and `state` for the panel, like Schwung's Tool tab.
- `src/web.go` serves `web_ui.html` unchanged, plus a small
  `window.schwungRemote` stand-in.

### core version

`src/go.mod` pins `core`. Until the Push Hack PR that adds pass-through
buttons and raw frames
([#33](https://github.com/federico-pepe/ableton-push-hack/pull/33)) is
merged and tagged, a `replace` points at that PR's commit on a fork. When
a `core/vX.Y.Z` tag carries it, pin the tag and delete the `replace`.

To work on `core` and Stacks together, a local `go.work` (not committed)
can point at a checkout next to this one. `make` mounts
`../ableton-push-hack` into the build container for that:

```
go 1.25.0

use ./src
use ../ableton-push-hack/core
```

### Releases

A `v*` tag that matches `hack.json`'s `version` runs
`.github/workflows/release.yml`. It tests, builds the static binary,
publishes `stacks.tar.gz` and updates `release.json`, which Push Hack
Catalog reads. CI (`ci.yml`) runs the tests on every push and checks that
`src/upstream/stacks/` matches schwung-stacks at `UPSTREAM_REF`.

## License

MIT, see [LICENSE](LICENSE). Third-party code and its licenses (Schwung's
headers, the Push Hack core, Go) are listed in
[THIRD_PARTY_LICENSES.md](THIRD_PARTY_LICENSES.md).
