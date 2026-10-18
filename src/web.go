package main

// web.go -- the Stacks browser panel, unchanged, behind a stand-in for
// Schwung's Tool tab.
//
// web_ui.html talks only to window.schwungRemote: `component`, setParam,
// onParamChange, resubscribe. The shim below provides those over HTTP, so
// the panel runs as it does in Schwung's manager. Param values arrive as
// the flat `state` map plus `rui_play` pushes (engine.go's poll).

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/federico-pepe/ableton-push-hack/core/httpx"
	"github.com/federico-pepe/ableton-push-hack/core/sse"
	"github.com/rgeissen/push-hack-stacks/dsp"
)

//go:embed upstream/stacks/src/web_ui.html
var panelHTML string

//go:embed upstream/stacks/src/module.json
var moduleJSON []byte

// remoteShim stands in for the manager's schwungRemote. It must run before
// the panel's own script, which reads `component` at load time.
const remoteShim = `<script>
(function () {
  var cbs = [], es = null;
  function deliver(p) { cbs.forEach(function (cb) { try { cb(p); } catch (e) { console.error(e); } }); }
  function refresh() {
    fetch("api/state").then(function (r) { return r.json(); }).then(deliver).catch(function () {});
  }
  function connect() {
    es = new EventSource("api/events");
    es.onmessage = function (e) { try { deliver(JSON.parse(e.data)); } catch (err) {} };
  }
  window.schwungRemote = {
    component: "tool",
    setParam: function (key, value) {
      fetch("api/param", { method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ key: key, value: String(value) }) }).catch(function () {});
    },
    onParamChange: function (cb) { cbs.push(cb); if (!es) connect(); },
    resubscribe: refresh,
    getChainParams: function () { return Promise.resolve([]); }
  };
})();
</script>`

// panelPage is web_ui.html with the shim placed first in <head>.
func panelPage() string {
	if i := strings.Index(panelHTML, "<head>"); i >= 0 {
		i += len("<head>")
		return panelHTML[:i] + "\n" + remoteShim + panelHTML[i:]
	}
	return remoteShim + panelHTML
}

func routes(e *Engine, broker *sse.Broker[map[string]string], ctl *surfaceCtl) http.Handler {
	page := panelPage()
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte(page))
	})
	mux.HandleFunc("GET /module.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(moduleJSON)
	})

	// The panel writes "overtake_dsp:<key>"; Stacks wants the bare key.
	mux.HandleFunc("POST /api/param", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Key, Value string }
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil || body.Key == "" {
			http.Error(w, "want {key, value}", http.StatusBadRequest)
			return
		}
		key := body.Key
		if i := strings.LastIndex(key, ":"); i >= 0 {
			key = key[i+1:]
		}
		if key == keyPlaysClosed && ctl != nil {
			ctl.setPlaysClosed(body.Value)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if (key == "undo" || key == "redo") && ctl != nil {
			ctl.scr.Undoing() // the panel's Undo gives the zoom back too
		}
		if !e.Call(func(i *dsp.Instance) { i.SetParam(key, body.Value) }, time.Second) {
			http.Error(w, "engine busy or not running", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// One read, for debugging from a shell: /api/param?key=prog
	mux.HandleFunc("GET /api/param", func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		var val string
		var ok bool
		if key == "" || !e.Call(func(i *dsp.Instance) { val, ok = i.GetParam(key) }, time.Second) || !ok {
			http.Error(w, "no such key, or engine not running", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(val))
	})

	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		m, _ := e.Snapshot()
		httpx.JSON(w, m)
	})

	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		ch := broker.Register()
		defer broker.Unregister(ch)
		if m, _ := e.Snapshot(); len(m) > 0 {
			ch <- m // the full picture first, then changes
		}
		sse.Serve(w, r, ch, func(m map[string]string) ([]byte, error) { return json.Marshal(m) })
	})

	// Take the Push or give it back, as Shift + Help and the LIVE tab do:
	// {"on": true, "tab": 2}. tab is the bank (0 MAIN .. 5 CLIP, 6 PATTERNS).
	mux.HandleFunc("POST /api/screen", func(w http.ResponseWriter, r *http.Request) {
		body := struct {
			On   bool   `json:"on"`
			Tab  *int   `json:"tab"`
			Mode string `json:"mode"` // "listen": the screen only (listen mode)
		}{}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
			http.Error(w, `want {"on": bool, "tab": n, "mode": "edit"|"listen"}`, http.StatusBadRequest)
			return
		}
		tab := -1
		if body.Tab != nil {
			tab = *body.Tab
		}
		m := modeLive
		switch {
		case body.On && body.Mode == "listen":
			m = modeListen
		case body.On:
			m = modeEdit
		}
		if err := ctl.setMode(m, tab); err != nil {
			http.Error(w, "push-manager: "+err.Error(), http.StatusBadGateway)
			return
		}
		on, t := ctl.scr.State()
		httpx.JSON(w, map[string]any{"on": on, "tab": t, "mode": modeNames[ctl.mode()]})
	})
	mux.HandleFunc("GET /api/screen", func(w http.ResponseWriter, r *http.Request) {
		on, t := ctl.scr.State()
		out := map[string]any{"on": on, "tab": t, "mode": modeNames[ctl.mode()]}
		for k, v := range ctl.View() {
			out[k] = v
		}
		httpx.JSON(w, out)
	})
	// What the pads and buttons were last sent, for tests.
	mux.HandleFunc("GET /api/leds", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, ctl.LEDs())
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, ready := e.Snapshot()
		httpx.JSON(w, map[string]bool{"engine": ready})
	})
	return mux
}
