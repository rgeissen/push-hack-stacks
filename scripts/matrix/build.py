"""Builds docs/control-matrix.html: the control matrix for Stacks on Push 3.

Run by `make matrix`. Inputs: the folder cmd/matrixdata wrote (data.json
and the pad-map renders, taken from the code that runs on Push), and the
Move matrix from schwung-stacks, whose stylesheet and knob texts this page
shares -- the knobs are the same params, and Push's differences are in
OVERRIDE below.

    python3 -I scripts/matrix/build.py <matrixdata dir> <stacks-matrix.html> <out.html> <git ref>
"""
import html
import json
import pathlib
import re
import sys

DATA = pathlib.Path(sys.argv[1])
MOVE = pathlib.Path(sys.argv[2])
OUT = pathlib.Path(sys.argv[3])
REF = sys.argv[4]

move = MOVE.read_text()
css_move = move[move.index("<style>") + len("<style>"):move.index("</style>")]


def move_banks(s):
    """The Move matrix's banks: each knob's key, description and range."""
    banks = []
    for sec in re.findall(r'<section class="bank[^"]*">(.*?)</section>', s, re.S):
        name = re.search(r"<h2>(.*?)</h2>", sec).group(1)
        m = re.search(r'<p class="scope">(.*?)</p>', sec, re.S)
        knobs = []
        for k in re.findall(r'<div class="knob">(.*?)</span></div>', sec + "</span></div>", re.S):
            f = dict(re.findall(r'<span class="(pos|code|name|desc|rng)">(.*?)</span>(?=<span class="(?:pos|code|name|desc|rng)"|$)',
                                k + '<span class="pos">', re.S))
            key = re.search(r'<span class="key">(.*?)</span>', k)
            knobs.append({"key": key.group(1) if key else None, "desc": f.get("desc"),
                          "rng": re.sub(r"\s+", " ", f.get("rng", ""))})
        banks.append({"bank": name, "scope": m.group(1) if m else "", "knobs": knobs})
    return banks


banks = move_banks(move)
data = json.loads((DATA / "data.json").read_text())
PORT = json.loads((pathlib.Path(__file__).resolve().parents[2] / "hack.json").read_text())["port"]
for _t in data["Tabs"]:
    _t["Knobs"] = _t["Knobs"] or []
    _t["Actions"] = _t["Actions"] or []
legend = {p: (DATA / f"legend_{p}.b64").read_text() for p in ("WRITE", "VOICE", "FEEL")}
listen_png = (DATA / "listen.b64").read_text()
missing = [k["Key"] for t in data["Tabs"] for k in t["Knobs"] if k["Key"]]

desc_by_key, rng_by_key = {}, {}
for b in banks:
    for k in b["knobs"]:
        if k["key"]:
            desc_by_key[k["key"]] = k["desc"]
            rng_by_key[k["key"]] = k["rng"]
scope_by_bank = {b["bank"]: b["scope"] for b in banks}
unknown = [k for k in missing if k not in desc_by_key]
if unknown:
    sys.exit(f"the Move matrix has no text for: {unknown} -- add it there or to OVERRIDE")

# Where Push differs from Move, the Push text wins.
OVERRIDE = {
    "read": "Not on Push yet. On Move it reads a clip from Move's own set file; Live's sets are a different format, so this will come through Browser Bridge. The encoder says <strong>NOT ON PUSH YET</strong>.",
    "read_mode": "Goes with Read Clip, so <strong>not on Push yet</strong>. On Move it chooses whether reading replaces the progression or appends to it.",
    "stamp": "Arms one lap for Live to record. Press <strong>Record</strong> on Push (it goes to Live) and Stacks plays the progression once, from Live's downbeat, into the selected track. The screen says <em>ARMED - PRESS RECORD</em>. Also on <strong>Capture</strong> and the CLIP tab's bottom row.",
    "run": "Whether Stacks plays along with Live's transport (Play on Push). Switched off after a stamp so the clip is not doubled. Closing Stacks also stops it playing along; opening it gives Run back.",
    "inv": "One ladder through zero: up rotates the lowest voice an octave up, down rotates the highest an octave down. Under a <em>dyn</em> Voice Grouping the encoder reads <strong>AUTO</strong>: voice leading picks the inversion, and the value you set waits for grouping to change.",
}

TAB_NOTE = {
    "MAIN": "Harmony and the clip: Key and Scale re-snap the progression as you turn them.",
    "START": "The libraries: turning one applies a progression.",
    "CHORD": "The selected chord: what it is and how long.",
    "VOICE": "The selected chord: how it is voiced and played.",
    "FEEL": "The whole progression's timing and touch.",
    "CLIP": "Performing and recording: Stamp, Clip Transpose, Clip Octave, Run.",
    "PATTERNS": "Sixteen buffers. The screen shows each slot's chords.",
    "LIVE": "Gives the surface back to Live. Shift + LIVE: listen mode, the screen only.",
}

ACTION_TEXT = {
    "INSERT": "Inserts a copy of the selected chord after it.",
    "DELETE": "Deletes the selected chord. On the PATTERNS tab this button is CLEAR SLOT instead.",
    "DUPLICATE": "Duplicates the whole progression, so you can vary the second half. The clip grows to hold it; nothing happens if it cannot.",
    "HALFTIME": "Every chord twice as long: the phrase at half speed. Refused if the clip cannot hold it.",
    "DOUBLE": "Double time: every chord half as long. Refused if a chord is already as short as it gets.",
    "MUTE": "Mutes the selected chord; it stays in the progression, drawn as an outline. Lit while the chord is muted.",
    "UNDO": "Steps back through the buffer.",
    "REDO": "Steps forward again.",
    "PREV OFF": "Editing a chord plays nothing. Lit when chosen.",
    "PREV CHORD": "Editing a chord plays it once. Lit when chosen.",
    "PREV LOOP": "Editing plays it, and the progression keeps looping while Live is stopped. Lit when chosen.",
    "RANDOM VEL": "Re-rolls the velocity deviations: same depth, a new feel.",
    "RANDOM TIME": "Re-rolls the timing spread, leaving velocity alone.",
    "STAMP": "Arms one lap for Live to record; press Record. Lit while armed.",
    "CLEAR": "Empties the progression to a single rest in your Key.",
    "RUN": "Stacks follows Live's transport. Lit while on.",
    "PLAY CLOSED": "Keeps Stacks playing while another app is on Push's screen, so it can follow Stacks' chords. Off by default: closed, Stacks is silent. Lit while on.",
    "CLEAR SLOT": "Arms a clear: the next pattern pad you tap is emptied. Press again to cancel. Lit while armed.",
    "COPY TO SLOT": "Arms a copy: the next pattern pad you tap gets the current pattern. Press again to cancel. Lit while armed.",
}

# Every other control, grouped by role. id -> (label, gesture text, LED text)
KEYS = {
    "edit": [
        ("shift", "Shift", "Modifier. Shift + Help opens Push Hack Home; Shift + LIVE switches to listen mode; Shift + Undo redoes; Shift + Double Loop is double time; Shift + jog changes tab; Shift + Scale follows Push's scale.", "white"),
        ("help", "Help", "Push-manager's: Shift + Help opens Push Hack Home, where STACKS opens Stacks; Shift + Help, Help opens the superstar. With an older Push Hack without Home: Live -> Stacks (edit) -> Stacks (listen) -> Live.", "white"),
        ("delete", "Delete", "Tap: deletes the selected chord. Hold and tap a pattern pad: clears that slot.", "white"),
        ("undo", "Undo", "Undo. With Shift: redo.", "white"),
        ("add", "Add", "Inserts a copy of the selected chord after it.", "white"),
        ("duplicate", "Duplicate", "Duplicates the whole progression.", "white"),
        ("double", "Double Loop", "Halftime: every chord twice as long. With Shift: double time.", "white"),
        ("mute", "Mute", "Mutes or unmutes the selected chord.", "white"),
        ("new", "New", "Clears the progression to a single rest.", "white"),
        ("capture", "Capture", "Stamp: arms one lap for Live to record.", "white"),
        ("octup", "Octave +", "Clip Octave up: everything that sounds, one octave.", "white"),
        ("octdn", "Octave -", "Clip Octave down.", "white"),
        ("scale", "Scale", "Jumps to the MAIN tab, where Key and Scale are. With Shift: Stacks' Key and Scale follow Push's scale (Browser Bridge 0.3 in Live); turning Stacks' Key or Scale stops following.", "white"),
    ],
    "nav": [
        ("up", "Up", "Previous editor page (WRITE, VOICE, FEEL). Lit only when there is one.", "cond"),
        ("down", "Down", "Next editor page. Lit only when there is one.", "cond"),
        ("left", "Left", "Selects the previous chord. Lit only when there is one.", "cond"),
        ("right", "Right", "Selects the next chord. Lit only when there is one.", "cond"),
        ("pgl", "Page <", "Scrolls the page's long pad row (Colour, Shape, Rhythm) back by eight. Lit only when it can.", "cond"),
        ("pgr", "Page >", "Scrolls the long pad row on by eight. Lit only when it can.", "cond"),
        ("jog", "Jog", "Turn: selects the chord. Shift + turn: changes tab. Press and hold: plays from the selected chord onward.", "none"),
    ],
    "live": [
        ("play", "Play", "Live's Play and Stop. Stacks follows Live's transport.", "live"),
        ("record", "Record", "Live's Record. With a stamp armed, Stacks plays one lap into it.", "live"),
        ("metro", "Metronome", "Live's metronome.", "live"),
        ("tap", "Tap Tempo", "Live's tap tempo.", "live"),
        ("tempo", "Tempo", "Live's tempo encoder, turn and press.", "live"),
        ("volume", "Volume", "Live's master volume encoder.", "live"),
    ],
}

PAD_ZONE = {
    0: ("CHORDS 1-8", "Tap selects the chord. Hold: it loops with its rhythm while you hold it, and the playhead runs inside it."),
    1: ("CHORDS 9-16", "Chords 9 to 16, the same as the row below."),
    6: ("PATTERNS 1-8", "Tap goes to the slot. Hold half a second: copies the current pattern into it. Delete + tap, or CLEAR SLOT then tap: clears it."),
    7: ("PATTERNS 9-16", "Slots 9 to 16, the same as the row below."),
}

ROW_TEXT = {
    "DEGREE": "Writes the selected chord's scale degree, I to VII, or makes it a rest.",
    "LENGTH": "Sets the chord's length: a quarter bar to eight bars.",
    "FAMILY": "Chooses the shape family: 5th, triad, 6th, 7th, 9th, extended.",
    "SHAPE": "The shapes in that family. Page arrows scroll when there are more than eight.",
    "INVERSION": "-3 to +3. Grey with no value lit when voice leading picks it (AUTO).",
    "COLOUR": "How the chord is voiced: close, open, drops, shell, rootless, quartal, spread, cluster.",
    "OCTAVE": "The chord's octave, 1 to 5.",
    "STRUM": "Spreads the notes' onsets, 0 to 100.",
    "RHYTHM": "The chord's figure in the bar: hold, quarters, son clave, tresillo and 31 more. Page arrows scroll.",
    "GATE": "How long the notes ring, against the progression's Gate.",
    "VELOCITY": "Chord velocity offset.",
    "OFFSET": "Pushes the chord later, in sixteenths of a bar.",
}


def e(s):
    return html.escape(s, quote=True)


def plain(s):
    return re.sub(r"<[^>]+>", "", s or "")


# ---- banks section ---------------------------------------------------------
bank_html = []
for i, t in enumerate(data["Tabs"][:6]):
    name = t["Name"]
    chordish = name in ("CHORD", "VOICE")
    labels = "".join(f"<span>{e(k['Label'])}</span>" for k in t["Knobs"])
    labels += "<span></span>" * (8 - len(t["Knobs"]))
    knobs = []
    for pos, k in enumerate(t["Knobs"], 1):
        if not k["Key"]:
            knobs.append('<div class="knob knob--empty" aria-hidden="true"></div>')
            continue
        d = OVERRIDE.get(k["Key"], desc_by_key.get(k["Key"], ""))
        trig = '<span class="trig">turn to fire</span>' if k["Trigger"] else ""
        rng = rng_by_key.get(k["Key"], "")
        knobs.append(
            f'<div class="knob"><span class="pos">{pos}</span>'
            f'<span class="code">{e(k["Label"])}{trig}</span>'
            f'<span class="desc">{d}</span><span class="rng">{rng}</span></div>')
    knobs += ['<div class="knob knob--empty" aria-hidden="true"></div>'] * (8 - len(t["Knobs"]))
    acts = "".join(f"<span>{e(a)}</span>" if a else "<span></span>" for a in t["Actions"])
    bank_html.append(f'''
<section class="bank {'bank--chord' if chordish else 'bank--prog'}" id="bank-{name.lower()}">
  <header class="bank__head">
    <h2>{e(name)}</h2>
    <p class="scope">{scope_by_bank.get(name, '')}</p>
    <p class="page">top button <code>{i + 1}</code> &middot; {len([k for k in t['Knobs'] if k['Key']])} of 8</p>
  </header>
  <div class="screen" aria-label="the encoder labels on Push's screen">{labels}</div>
  <div class="grid">{''.join(knobs)}</div>
  <div class="screen screen--acts" aria-label="the bottom button row on this tab">{acts}</div>
</section>''')

# ---- pad pages ---------------------------------------------------------------
page_html = []
for pg in ("WRITE", "VOICE", "FEEL"):
    rows = data["Pages"][pg]
    trs = []
    for r in range(7, -1, -1):
        row = rows[r]
        cells = "".join(
            f'<td><span class="chip" style="--c:{c or "transparent"}">{e(l)}</span></td>' if l else "<td></td>"
            for l, c in zip(row["Labels"], row["Colors"]))
        trs.append(f'<tr><th>{e(row["Name"])}</th>{cells}</tr>')
    scene = {"WRITE": "1/4", "VOICE": "1/4t", "FEEL": "1/8"}[pg]
    page_html.append(f'''
<figure class="padpage">
  <figcaption><b>{pg}</b> &middot; scene button <code>{scene}</code></figcaption>
  <img class="legend-img" alt="Push's screen while the {scene} button is held: the {pg} pad map" src="data:image/png;base64,{legend[pg]}">
  <div class="tablewrap"><table class="padtable"><tbody>{''.join(trs)}</tbody></table></div>
</figure>''')

# ---- buttons table ------------------------------------------------------------
LEDTXT = {"white": "white", "cond": "white when it can act, dark otherwise", "none": "no LED", "live": "Live's own"}
btn_rows = []
for group, title in (("edit", "Edit keys"), ("nav", "Navigation"), ("live", "Live keeps these")):
    btn_rows.append(f'<tr class="grp"><th colspan="3">{title}</th></tr>')
    for _id, label, text, led in KEYS[group]:
        btn_rows.append(f'<tr><th>{e(label)}</th><td>{e(text)}</td><td class="note">{LEDTXT[led]}</td></tr>')
btn_rows.insert(0, '<tr class="grp"><th colspan="3">Screen buttons and scene column</th></tr>'
    '<tr><th>Top 1-7</th><td>Tabs: MAIN, START, CHORD, VOICE, FEEL, CLIP, PATTERNS. They pick what the encoders and the bottom row do.</td><td class="note">amber on the tab you are on, white on the others</td></tr>'
    '<tr><th>Top 8</th><td>LIVE: gives the surface back to Live, on the release. Stacks stops playing along. Shift + LIVE: listen mode (the screen only; pads and buttons with Live).</td><td class="note">white</td></tr>'
    '<tr><th>Bottom 1-8</th><td>The tab\'s actions; see each bank above.</td><td class="note">white; amber while that action is on or armed</td></tr>'
    '<tr><th>1/4, 1/4t, 1/8</th><td>The pad page: WRITE, VOICE, FEEL. Hold to see the page\'s pad map on the screen.</td><td class="note">amber on the page you are on, white on the others</td></tr>'
    '<tr><th>Encoders 1-8</th><td>The tab\'s parameters. Touch one and its label lights. A step takes six ticks on a short list, three on a medium one, two on a long one; only the long lists accelerate. A trigger fires once per turn.</td><td class="note">no LED</td></tr>')

# ---- the interactive map's data ---------------------------------------------
mapdata = {
    "tabs": [{"name": t["Name"], "note": TAB_NOTE.get(t["Name"], ""),
              "knobs": [{"label": k["Label"], "text": plain(OVERRIDE.get(k["Key"], desc_by_key.get(k["Key"], ""))),
                         "trigger": bool(k["Trigger"])} for k in t["Knobs"]],
              "actions": [{"label": a, "text": ACTION_TEXT.get(a, "")} for a in t["Actions"]],
              "screen": t["Screen"]} for t in data["Tabs"]],
    "pages": {pg: [{"name": r["Name"], "labels": r["Labels"], "colors": r["Colors"]} for r in data["Pages"][pg]]
              for pg in ("WRITE", "VOICE", "FEEL")},
    "keys": {g: [{"id": i, "label": l, "text": t, "led": led} for i, l, t, led in v] for g, v in KEYS.items()},
    "zones": {str(k): {"name": v[0], "text": v[1]} for k, v in PAD_ZONE.items()},
    "rowText": ROW_TEXT,
}

css_push = r"""
.ports { margin:22px 0 0; padding:16px 18px; background:var(--panel); border:1px solid var(--line); border-radius:10px; }
.ports__head { margin:0 0 12px; font:600 11px/1 "IBM Plex Mono", ui-monospace, monospace; letter-spacing:.08em; text-transform:uppercase; color:var(--muted); }
.ports dl { margin:0; display:grid; grid-template-columns:repeat(auto-fit, minmax(220px, 1fr)); gap:14px 28px; }
.ports dt { font:600 14px/1.3 "Chakra Petch", ui-sans-serif, sans-serif; color:var(--ink); }
.ports dd { margin:4px 0 0; font-size:13px; line-height:1.4; color:var(--muted); }
.ports dd code { display:block; margin-bottom:3px; font:600 17px/1.3 "IBM Plex Mono", ui-monospace, monospace; color:var(--accent); }
.ports .own dd code { font-size:20px; }
.ports__note { margin:14px 0 0; max-width:none; font-size:12.5px; color:var(--muted); }

/* ---- Push additions: the surface map ------------------------------------
   Layout: the hardware's own order, top to bottom -- encoders, top buttons,
   screen, bottom buttons, pads with the scene column -- then the keys by
   role. Schematic, not to scale. The surface is dark in both themes, like
   the screen rows: it is a picture of black hardware. */
:root { --hw:#14191E; --hw-edge:#262E36; --cap:#1E252C; --cap-ink:#9FB0BE;
  --lit:#E9EEF2; --amber:#FFB454; --live:#4E9DBA; }
.surface { background:var(--hw); border:1px solid var(--line); border-radius:6px;
  box-shadow:var(--shadow); padding:18px; display:grid; gap:10px; color:var(--cap-ink); }
.row8 { display:grid; grid-template-columns:repeat(8,minmax(0,1fr)); gap:6px; }
.enc { display:flex; flex-direction:column; align-items:center; gap:5px; min-width:0; }
.enc i { width:26px; height:26px; border-radius:50%; background:radial-gradient(circle at 40% 35%, #3A444E, #12171C 70%);
  border:1px solid var(--hw-edge); }
.enc span { font:500 9.5px/1.15 "IBM Plex Mono", monospace; letter-spacing:.04em; text-align:center;
  color:var(--cap-ink); min-height:2.3em; overflow-wrap:anywhere; }
.cap { appearance:none; border:1px solid var(--hw-edge); background:var(--cap); color:var(--cap-ink);
  border-radius:3px; padding:6px 2px; min-width:0; cursor:pointer;
  font:500 10px/1.2 "IBM Plex Mono", monospace; letter-spacing:.05em; text-transform:uppercase;
  overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
.cap:focus-visible, .pad:focus-visible, .enc:focus-visible { outline:2px solid var(--amber); outline-offset:2px; }
.cap.is-lit { color:var(--lit); border-color:#56626D; box-shadow:inset 0 0 0 1px rgba(233,238,242,.12); }
.cap.is-amber { background:var(--amber); border-color:var(--amber); color:#14191E; font-weight:700; }
.cap.is-live { border-color:var(--live); color:var(--live); }
.cap.is-dark { opacity:.42; }
.cap:empty { cursor:default; }
.shot { display:block; width:100%; height:auto; border-radius:2px; border:1px solid var(--hw-edge);
  image-rendering:pixelated; background:#000; }
.deck { display:grid; grid-template-columns:minmax(0,1fr) 54px; gap:10px; align-items:start; }
.pads { display:grid; grid-template-columns:repeat(8,minmax(0,1fr)); gap:5px; }
.pad { aspect-ratio:1; border-radius:4px; border:1px solid var(--hw-edge); background:var(--cap);
  display:flex; align-items:center; justify-content:center; padding:2px; min-width:0; cursor:pointer;
  font:600 9px/1.05 "IBM Plex Mono", monospace; text-align:center; overflow:hidden; color:rgba(0,0,0,.72);
  overflow-wrap:anywhere; }
.pad.is-off { color:var(--cap-ink); opacity:.55; font-weight:500; }
.pad.zone-gap { margin-top:6px; }
.scenes { display:grid; gap:5px; }
.scenes .cap { aspect-ratio:1; padding:2px; white-space:normal; font-size:9px; }
.keys { display:grid; gap:8px; }
.keys h4 { margin:4px 0 0; font:500 10px/1 "IBM Plex Mono", monospace; letter-spacing:.14em;
  text-transform:uppercase; color:#6F7F8C; }
.keyrow { display:flex; flex-wrap:wrap; gap:6px; }
.keyrow .cap { padding:6px 9px; }
.inspector { margin-top:12px; background:var(--panel); border:1px solid var(--line); border-left:3px solid var(--accent);
  border-radius:3px; padding:14px 18px; min-height:96px; box-shadow:var(--shadow); }
.inspector .what { font:700 18px/1.2 "Chakra Petch", sans-serif; letter-spacing:.03em; margin:0 0 4px; }
.inspector .where { font:400 11px/1.3 "IBM Plex Mono", monospace; color:var(--muted); margin:0 0 8px;
  letter-spacing:.06em; text-transform:uppercase; }
.inspector p.text { margin:0; max-width:70ch; }
.legendkey { display:flex; flex-wrap:wrap; gap:16px; margin:12px 0 0;
  font:400 12px/1.4 "IBM Plex Mono", monospace; color:var(--muted); }
.legendkey span { display:inline-flex; align-items:center; gap:7px; }
.lk { width:14px; height:14px; border-radius:3px; border:1px solid var(--line); display:inline-block; }
.lk--amber { background:#FFB454; border-color:#FFB454; } .lk--lit { background:#E9EEF2; }
.lk--dark { background:#1E252C; } .lk--live { background:transparent; border:2px solid #4E9DBA; }
.screen--acts { border-bottom:0; border-top:1px solid var(--line); }
.screen--acts span { color:#8FA3B3; font-size:11px; }
.padpage { margin:0 0 22px; background:var(--panel); border:1px solid var(--line); border-radius:3px;
  box-shadow:var(--shadow); overflow:hidden; }
.padpage figcaption { padding:12px 18px; font:500 12px/1 "IBM Plex Mono", monospace; color:var(--muted);
  border-bottom:1px solid var(--line); }
.padpage figcaption b { font:700 16px/1 "Chakra Petch", sans-serif; color:var(--accent); letter-spacing:.05em; margin-right:6px; }
.legend-img { display:block; width:100%; height:auto; image-rendering:pixelated; background:#000; }
.tablewrap { overflow-x:auto; }
.padtable { border:0; border-radius:0; box-shadow:none; min-width:640px; }
.padtable th { font:500 11px/1.3 "IBM Plex Mono", monospace; color:var(--muted); white-space:nowrap; width:1%; }
.padtable td { padding:6px 4px; }
.chip { display:block; text-align:center; padding:5px 2px; border-radius:3px; font:600 10px/1.1 "IBM Plex Mono", monospace;
  background:var(--c); color:#101418; border:1px solid rgba(0,0,0,.15); overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
.chip[style*="transparent"] { background:var(--ground); color:var(--muted); border-color:var(--line); }
.btns tr.grp th { font:500 11px/1 "IBM Plex Mono", monospace; letter-spacing:.14em; text-transform:uppercase;
  color:var(--muted); background:var(--ground); padding-top:14px; }
.lede { max-width:68ch; color:var(--muted); margin:0 0 18px; }
@media (max-width:720px) {
  .surface { padding:10px; gap:8px; }
  .row8 { gap:3px; } .pads { gap:3px; } .deck { grid-template-columns:minmax(0,1fr) 40px; gap:6px; }
  .cap { font-size:8.5px; padding:5px 1px; } .enc span { font-size:8px; } .enc i { width:18px; height:18px; }
  .pad { font-size:7px; border-radius:3px; } .scenes .cap { font-size:7.5px; }
}
@media (prefers-reduced-motion: reduce) { * { transition:none !important; } }
"""

script = r"""
(() => {
  const D = JSON.parse(document.getElementById('matrix-data').textContent);
  const $ = (s, el = document) => el.querySelector(s);
  const st = { tab: 2, page: 'WRITE' };
  const ins = $('#inspector');
  const say = (what, where, text) => {
    $('.what', ins).textContent = what; $('.where', ins).textContent = where; $('.text', ins).textContent = text;
  };
  const bind = (el, f) => { el.addEventListener('mouseenter', f); el.addEventListener('focus', f); el.addEventListener('click', f); };

  function drawTab() {
    const t = D.tabs[st.tab];
    const encs = $('#encs'); encs.textContent = '';
    for (let i = 0; i < 8; i++) {
      const k = t.knobs[i];
      const d = document.createElement('div'); d.className = 'enc'; d.tabIndex = 0;
      d.innerHTML = '<i></i><span></span>';
      d.querySelector('span').textContent = k && k.label ? k.label : '';
      bind(d, () => k && k.label
        ? say(k.label, 'encoder ' + (i + 1) + ' · ' + t.name + (k.trigger ? ' · turn to fire' : ''), k.text)
        : say('Encoder ' + (i + 1), t.name, 'Nothing on this encoder on this tab.'));
      encs.appendChild(d);
    }
    document.querySelectorAll('#tabs .cap').forEach((b, i) => {
      b.classList.toggle('is-amber', i === st.tab); b.classList.toggle('is-lit', i !== st.tab);
    });
    const acts = $('#acts'); acts.textContent = '';
    for (let i = 0; i < 8; i++) {
      const a = t.actions ? t.actions[i] : null;
      const b = document.createElement('button'); b.className = 'cap ' + (a && a.label ? 'is-lit' : 'is-dark');
      b.type = 'button'; b.textContent = a && a.label ? a.label : '';
      b.setAttribute('aria-label', a && a.label ? a.label : 'bottom button ' + (i + 1) + ', unused');
      bind(b, () => a && a.label
        ? say(a.label, 'bottom button ' + (i + 1) + ' · ' + t.name, a.text)
        : say('Bottom button ' + (i + 1), t.name, 'Nothing on this tab: dark.'));
      acts.appendChild(b);
    }
    const shot = $('#shot');
    if (t.screen) { shot.src = 'data:image/png;base64,' + t.screen; shot.alt = 'Push screen on the ' + t.name + ' tab'; }
  }

  function drawPage() {
    const rows = D.pages[st.page];
    const pads = $('#pads'); pads.textContent = '';
    for (let r = 7; r >= 0; r--) {
      for (let c = 0; c < 8; c++) {
        const row = rows[r], label = row.labels[c], color = row.colors[c];
        const p = document.createElement('button'); p.type = 'button';
        p.className = 'pad' + (color ? '' : ' is-off') + ((r === 5 || r === 1) ? ' zone-gap' : '');
        if (color) p.style.background = color;
        p.textContent = label || '';
        p.setAttribute('aria-label', row.name + ', pad ' + (c + 1) + (label ? ': ' + label : ''));
        const z = D.zones[String(r)];
        bind(p, () => {
          if (z) return say(label ? z.name + ' · ' + label : z.name, 'pad row ' + (r + 1) + ' · every page', z.text);
          const base = row.name.split(' ')[0];
          say(label ? row.name + ' · ' + label : row.name, 'pad row ' + (r + 1) + ' · ' + st.page + ' page',
              (D.rowText[base] || '') + (label ? ' This pad writes ' + label + ' to the selected chord.' : ''));
        });
        pads.appendChild(p);
      }
    }
    document.querySelectorAll('#scenes .cap').forEach(b => {
      const on = b.dataset.page === st.page;
      b.classList.toggle('is-amber', on); b.classList.toggle('is-lit', !!b.dataset.page && !on);
    });
  }

  document.querySelectorAll('#tabs .cap').forEach((b, i) => {
    b.addEventListener('click', () => {
      if (i < 7) { st.tab = i; drawTab(); }
      const t = D.tabs[i];
      say(t.name, 'top button ' + (i + 1), t.note);
    });
    bind(b, () => say(D.tabs[i].name, 'top button ' + (i + 1), D.tabs[i].note));
  });
  document.querySelectorAll('#scenes .cap').forEach(b => {
    if (!b.dataset.page) { bind(b, () => say('Scene button', 'scene column', 'Not used by Stacks: dark.')); return; }
    b.addEventListener('click', () => { st.page = b.dataset.page; drawPage(); });
    bind(b, () => say(b.dataset.page + ' page', 'scene button ' + b.dataset.scene,
      'Shows the ' + b.dataset.page + ' rows on pad rows 3 to 6. Hold it to see this page’s pad map on the screen.'));
  });
  for (const [g, ks] of Object.entries(D.keys)) {
    const row = document.getElementById('keys-' + g);
    for (const k of ks) {
      const b = document.createElement('button'); b.type = 'button';
      b.className = 'cap ' + (k.led === 'live' ? 'is-live' : k.led === 'none' ? '' : 'is-lit');
      b.textContent = k.label;
      bind(b, () => say(k.label, g === 'live' ? 'passes through to Live' : g === 'nav' ? 'navigation' : 'edit key', k.text));
      row.appendChild(b);
    }
  }
  bind($('#shot'), () => say('The screen', D.tabs[st.tab].name + ' tab',
    'Tabs on top, the encoders’ names and values, the progression in true time with its notes on the scale’s lanes, and the bottom row’s actions.'));
  drawTab(); drawPage();
  say('Point at any control', 'the surface map', 'Hover, tap or tab to a control to see what it does. The top buttons switch the tab, the 1/4, 1/4t and 1/8 buttons switch the pad page.');
})();
"""

tabs_caps = "".join(
    f'<button type="button" class="cap">{e(t["Name"])}</button>' for t in data["Tabs"])
scenes = "".join(
    f'<button type="button" class="cap" data-page="{p}" data-scene="{s}">{s}</button>'
    for p, s in (("WRITE", "1/4"), ("VOICE", "1/4t"), ("FEEL", "1/8"))) + \
    "".join(f'<button type="button" class="cap is-dark" aria-label="unused scene button">{s}</button>'
            for s in ("1/8t", "1/16", "1/16t", "1/32", "1/32t"))

notes = [
    ("Open it from Push Hack Home.", "Shift + Help opens Home, push-manager's switcher; pick STACKS. Star it there, or make it the superstar to open it with Shift + Help, Help. Shift + LIVE switches to listen mode; LIVE gives everything back. With an older Push Hack without Home, Shift + Help cycles Live, editing and listening, as in 0.2."),
    ("Live keeps the transport.", "Play, Record, Metronome, Tap Tempo, the tempo and volume encoders go to Live, with Live's own LEDs. Stacks follows Live's clock."),
    ("Closed is parked.", "When you close Stacks it stops playing along with Live; opening gives Run back. A stamp that is armed still plays."),
    ("Hold a chord pad to hear it.", "The chord loops with its rhythm while you hold the pad, and a playhead runs inside it. Tapping only selects."),
    ("Hold a scene button for the pad map.", "The screen shows every pad row of that page, named, in the pads' own colours, until you let go."),
    ("Lit means it does something.", "Amber is where you are or what is on. White is available. Dark has no meaning here. The arrows light only when they can move."),
    ("Inversion can read AUTO.", "Under a dyn Voice Grouping, voice leading picks each chord's inversion. The first chord, and a chord after a rest, keep their own."),
    ("The lanes hold still.", "Voicing edits never zoom the staff in; it zooms out only to keep notes on screen, and moves by whole octaves. The bar at the right edge shows the register."),
    ("Slots show their chords.", "A used pattern slot shows its chord count, bars and first chords. CLEAR SLOT or COPY TO SLOT, then tap the slot."),
    ("Sync once, in Live.", "Settings > MIDI > Stacks In > Sync on. Without it Stacks cannot follow Play and Stop; the screen says so while Run is on."),
    ("An older Push Hack is named, not hidden.", "If push-manager lacks pass-through buttons or raw frames, Shift + Help shows what to update. Playing into Live still works."),
    ("Shift + Scale follows Push's scale.", "Stacks' Key and Scale follow the scale set with Push's Scale button, marked FROM PUSH on MAIN. A scale Stacks lacks keeps Stacks' and follows the key. Needs Browser Bridge 0.3, active in Live."),
    ("Listen mode names what you play.", "Your pads stay with Live; the screen names the chord, with its degree, inversion and what is left out. Played voicings count: no 5th, a slash bass, add11, 7#11, two notes as an interval. The name clears when you let go; LAST, a small line under the notes, keeps the chord you played before."),
    ("The power button always works.", "push-manager gives the screen and buttons back to Push the moment you press it, and at every start, so Live's own questions can be answered."),
    ("Two ports, found by Live.", "Stacks Out carries the chords to the selected track. Stacks In carries Live's clock in."),
]
notes_html = "".join(f'<div class="note-card"><h3>{e(h)}</h3><p>{e(p)}</p></div>' for h, p in notes)

page = f"""<title>Stacks Push Matrix</title>
<meta name="claude:viewport-fit-sides" content="cover">
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Chakra+Petch:wght@500;700&family=IBM+Plex+Mono:wght@400;500;600&family=IBM+Plex+Sans:wght@400;500;600&display=swap">
<style>{css_move}
.wrap {{ padding-left:max(16px, env(safe-area-inset-left, 0px)); padding-right:max(16px, env(safe-area-inset-right, 0px)); }}
{css_push}</style>

<div class="wrap">
  <header class="masthead">
    <div>
      <h1>Stacks</h1>
      <p>Control matrix for the chord-progression tool on <strong>Ableton Push 3</strong>, a Push Hack catalog hack.
         Open it from <strong>Push Hack Home</strong> (<strong>Shift&nbsp;+&nbsp;Help</strong>): it takes Push's screen, encoders, buttons and pads; Live keeps the transport.
         The same six banks as on Move, with full names on a 960&times;160 screen and 64 pads that write the selected chord.</p>
    </div>
    <div class="stats">
      <span><b>7</b>tabs</span>
      <span><b>43</b>encoder params</span>
      <span><b>64</b>pads</span>
      <span><b>6</b>kept by Live</span>
    </div>
  </header>

  <section class="ports" aria-label="Web interfaces">
    <p class="ports__head">Web interfaces &middot; open in a browser on the same network</p>
    <dl>
      <div class="own"><dt>Stacks panel</dt><dd><code>http://&lt;push&gt;:{PORT}</code>The same browser panel as Stacks on Move, for the progression you play on Push. Every region and control of it: <em>Stacks Panel</em> (schwung-stacks, docs/panel.html).</dd></div>
      <div><dt>Push Manager</dt><dd><code>http://&lt;push&gt;:7701</code>Fixed port. Files, display, MIDI, and every installed hack with the port it got.</dd></div>
      <div><dt>Push Hack Catalog</dt><dd><code>http://&lt;push&gt;:7702</code>Fixed port. Installs, updates and removes hacks.</dd></div>
    </dl>
    <p class="ports__note"><code>&lt;push&gt;</code> is your Push's network address, for example <code>push.local</code>. {PORT} is the port in Stacks' <code>hack.json</code>. Push Hack Catalog gives each hack a port when it installs it, from 7711 up, so on your Push it can be another one: Push Manager lists it.</p>
  </section>

  <p class="eyebrow">The surface</p>
  <p class="lede">Schematic, not to scale, in the hardware's order. Every label, colour and screen here comes from the code that runs on Push: the screens are the device renderer's own output for one progression (Axis, in A).</p>
  <div class="surface" aria-label="Push 3 surface map">
    <div class="row8" id="encs"></div>
    <div class="row8" id="tabs">{tabs_caps}</div>
    <img class="shot" id="shot" tabindex="0" width="960" height="160" alt="">
    <div class="row8" id="acts"></div>
    <div class="deck">
      <div class="pads" id="pads"></div>
      <div class="scenes" id="scenes">{scenes}</div>
    </div>
    <div class="keys">
      <h4>Edit keys</h4><div class="keyrow" id="keys-edit"></div>
      <h4>Navigation</h4><div class="keyrow" id="keys-nav"></div>
      <h4>Live keeps these</h4><div class="keyrow" id="keys-live"></div>
    </div>
  </div>
  <div class="inspector" id="inspector" aria-live="polite"><p class="where"></p><p class="what"></p><p class="text"></p></div>
  <div class="legendkey">
    <span><i class="lk lk--amber"></i>where you are, or on</span>
    <span><i class="lk lk--lit"></i>available</span>
    <span><i class="lk lk--dark"></i>no meaning here: dark</span>
    <span><i class="lk lk--live"></i>Live's own</span>
  </div>

  <p class="eyebrow">Banks &mdash; top buttons 1 to 6</p>
  {''.join(bank_html)}

  <p class="eyebrow">Pads &mdash; three pages, picked by the scene buttons</p>
  <p class="lede">The bottom two rows are always the chords and the top two always the patterns. The middle four write the selected chord, and change with the page. Holding a scene button shows the screen below; the table is the same map, with the pads' LED colours.</p>
  {''.join(page_html)}

  <p class="eyebrow">Listen mode &mdash; Shift + LIVE</p>
  <p class="lede">The pads, buttons and encoders stay with Live: play your instrument as usual. The screen names the chord being played, on split-flap tiles that flip as you add a note (C, CMAJ, CMAJ7), with its degree and role in the key, its inversion or slash bass, and what a voicing leaves out. One tile per note drops in and latches; the keys go down, the root in amber. Below: C E B in A minor, a Cmaj7 without its 5th.</p>
  <figure class="padpage">
    <figcaption><b>LISTEN</b> &middot; the device renderer's own frame</figcaption>
    <img class="legend-img" alt="Push's screen in listen mode: CMAJ7 on flap tiles, degree III, tonic, root position, no 5th; tiles R, 3, 7; a keyboard with C, E and B down" src="data:image/png;base64,{listen_png}">
  </figure>
  <div class="tablewrap"><table class="btns"><tbody>
    <tr class="grp"><th colspan="2">Once, before the first use</th></tr>
    <tr><th>Browser Bridge</th><td>Version 0.4 or later, active in a Control Scripts slot (Settings &gt; MIDI). It keeps a silent track, <code>Stacks Listen</code>, whose input follows the track you select.</td></tr>
    <tr><th>Stacks Listen</th><td>In Settings &gt; MIDI, switch <b>Track</b> on for the output <code>Stacks Listen</code>. The screen says so while it is off.</td></tr>
  </tbody></table></div>

  <p class="eyebrow">Every button</p>
  <div class="tablewrap"><table class="btns"><tbody>{''.join(btn_rows)}</tbody></table></div>

  <p class="eyebrow">Worth knowing</p>
  <div class="notes">{notes_html}</div>

  <footer>
    Generated from <a href="https://github.com/rgeissen/push-hack-stacks">push-hack-stacks</a> at {e(REF)}.
    Knob texts shared with the <a href="https://github.com/rgeissen/schwung-stacks">Move matrix</a>, with Push's differences.
  </footer>
</div>
<script type="application/json" id="matrix-data">{json.dumps(mapdata).replace("</", "<\\/")}</script>
<script>{script}</script>
"""
OUT.parent.mkdir(parents=True, exist_ok=True)
OUT.write_text(page)
print(OUT, len(page))
