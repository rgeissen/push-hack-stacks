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
- **Top two pad rows** are the 16 patterns.
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

- Push 3 Standalone with Push Hack installed (Push Manager on port 7701).
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

`src/go.mod` pins `core` to a tagged version. Until a tag carries the new
raw-MIDI helpers ([#31](https://github.com/federico-pepe/ableton-push-hack/pull/31),
[#32](https://github.com/federico-pepe/ableton-push-hack/pull/32)), a local
`go.work` (not committed) points at `../ableton-push-hack/core`:

```
go 1.25.0

use ./src
use ../ableton-push-hack/core
```
