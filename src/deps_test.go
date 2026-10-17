package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/federico-pepe/ableton-push-hack/core/pmclient"
)

// Each state a user can be in, and what Stacks tells them.
func TestDepsSaysWhatIsMissing(t *testing.T) {
	stub := func(display, filter string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/display/status":
				w.Write([]byte(display))
			case "/api/midi/filter/status":
				w.Write([]byte(filter))
			default:
				http.NotFound(w, r)
			}
		}))
	}
	for _, tc := range []struct {
		name, display, filter, want string
	}{
		{"current release", `{"connected":true,"mode":0}`, `{"enabled":false,"pass_cc":[]}`, ""},
		{"older release", `{"connected":true}`, `{"enabled":false,"available":true}`, "update push hack"},
		{"no framebuffer", `{"connected":false}`, `{"enabled":false,"pass_cc":[]}`, "push-display"},
	} {
		srv := stub(tc.display, tc.filter)
		got := newDeps(pmclient.New(srv.URL)).Problem()
		srv.Close()
		if (tc.want == "") != (got == "") || !strings.Contains(strings.ToLower(got), tc.want) {
			t.Errorf("%s: problem %q, want one mentioning %q", tc.name, got, tc.want)
		}
	}
	if p := newDeps(pmclient.New("http://127.0.0.1:1")).Problem(); !strings.Contains(p, "not reachable") {
		t.Errorf("no push-manager: %q", p)
	}
}
