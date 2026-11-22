package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func r17RouteGens(t *testing.T, handler http.HandlerFunc) (*routeGens, *atomic.Int64) {
	t.Helper()
	api := httptest.NewServer(handler)
	t.Cleanup(api.Close)
	clock := new(atomic.Int64)
	clock.Store(time.Now().UnixNano())
	g := newRouteGens(api.URL)
	g.now = func() time.Time { return time.Unix(0, clock.Load()) }
	return g, clock
}

func TestR17RevocationFreshnessHTTP(t *testing.T) {
	for _, failure := range []string{"500", "404", "malformed"} {
		t.Run(failure, func(t *testing.T) {
			var mode atomic.Int32
			gens, clock := r17RouteGens(t, func(w http.ResponseWriter, r *http.Request) {
				switch mode.Load() {
				case 1:
					if failure == "malformed" {
						io.WriteString(w, "{")
					} else {
						status, _ := strconv.Atoi(failure)
						w.WriteHeader(status)
					}
				case 2:
					io.WriteString(w, `{"gens":{"abc-1":1},"desktops":["abc-1"]}`)
				default:
					io.WriteString(w, `{"gens":{},"desktops":["abc-1"]}`)
				}
			})
			g, _, port := setup(t)
			g.gens, g.now = gens, gens.now
			server := httptest.NewServer(g)
			t.Cleanup(server.Close)
			privateHost := strconv.Itoa(port) + "-abc-1.vm.boatvms.test"
			oldPort := first(tok(func(e int64) string { return signRoute(g.k(purposePortRoute), port, "abc-1", 0, e) }, time.Hour))
			oldDesktop := first(tok(func(e int64) string { return signDesktop(g.k(purposeDesktopLink), "abc-1", 0, e) }, time.Hour))
			read := func(host, name, token string, want int) {
				t.Helper()
				r, err := http.NewRequestWithContext(t.Context(), "GET", server.URL+"/private-secret", nil)
				if err != nil {
					t.Fatal(err)
				}
				r.Host = host
				r.AddCookie(&http.Cookie{Name: name, Value: token})
				resp, err := server.Client().Do(r)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				if resp.StatusCode != want || (want == 200) != strings.Contains(string(body), "path=/private-secret") {
					t.Errorf("read %s: %d %q, want %d", host, resp.StatusCode, body, want)
				}
			}
			ask := func(want int) {
				t.Helper()
				w := httptest.NewRecorder()
				g.ask(w, httptest.NewRequest("GET", "/ask?domain=abc-1.desktop.boat.test", nil))
				if w.Code != want {
					t.Errorf("desktop certificate approval: %d, want %d", w.Code, want)
				}
			}
			checkPrivate := func(want int) {
				read(privateHost, routeCookie, oldPort, want)
				read("abc-1.desktop.boat.test", desktopCookie, oldDesktop, want)
			}
			mode.Store(1)
			checkPrivate(503)
			ask(404)
			mode.Store(0)
			checkPrivate(200)
			ask(200)
			mode.Store(1) // The revocation occurs while boat-api cannot answer.
			maxAge := 2*routeGenPoll + routeGenTimeout
			clock.Add(int64(maxAge - time.Nanosecond))
			for range 3 {
				if err := gens.refresh(t.Context()); err == nil {
					t.Fatal("failed refresh accepted")
				}
			}
			checkPrivate(200)
			ask(200)
			clock.Add(1)
			checkPrivate(503)
			ask(404)
			read(strconv.Itoa(port)+"-xyz-2.vm.boatvms.test", "app", "1", 200)
			mode.Store(2)
			checkPrivate(401) // The stale cache refreshes; old cookies are revoked.
			freshPort := first(tok(func(e int64) string { return signRoute(g.k(purposePortRoute), port, "abc-1", 1, e) }, time.Hour))
			freshDesktop := first(tok(func(e int64) string { return signDesktop(g.k(purposeDesktopLink), "abc-1", 1, e) }, time.Hour))
			read(privateHost, routeCookie, freshPort, 200)
			read("abc-1.desktop.boat.test", desktopCookie, freshDesktop, 200)
			ask(200)
		})
	}
}

func TestR17SlowRefreshDoesNotRenewOldSnapshot(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	g, clock := r17RouteGens(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			io.WriteString(w, `{"gens":{},"desktops":["abc-1"]}`)
			return
		}
		w.WriteHeader(500)
	})
	done := make(chan error, 1)
	go func() { done <- g.refresh(t.Context()) }()
	<-entered
	clock.Add(int64(2*routeGenPoll + routeGenTimeout))
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, ok := g.gen(t.Context(), "abc-1"); ok {
		t.Error("a response already stale at completion authorizes a token")
	}
	if g.hasDesktop(t.Context(), "abc-1") {
		t.Error("a response already stale at completion authorizes a desktop certificate")
	}
}

func TestR17OutOfOrderRefreshKeepsNewerSnapshot(t *testing.T) {
	for _, oldFails := range []bool{false, true} {
		t.Run(strconv.FormatBool(oldFails), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			g, _ := r17RouteGens(t, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					close(entered)
					<-release
					if oldFails {
						w.WriteHeader(500)
					} else {
						io.WriteString(w, `{"gens":{},"desktops":["old"]}`)
					}
					return
				}
				io.WriteString(w, `{"gens":{"abc-1":1},"desktops":["abc-1"]}`)
			})
			done := make(chan bool, 1)
			go func() { _, ok := g.gen(t.Context(), "abc-1"); done <- ok }()
			<-entered
			err := g.refresh(t.Context()) // Same clock instant, but a later request.
			close(release)
			ok := <-done
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Error("an older failed refresh hides a newer fresh snapshot")
			}
			if gen, ok := g.gen(t.Context(), "abc-1"); !ok || gen != 1 {
				t.Errorf("newer revocation overwritten: gen=%d ready=%v", gen, ok)
			}
			if !g.hasDesktop(t.Context(), "abc-1") {
				t.Error("newer desktop table overwritten")
			}
		})
	}
}

func TestR17CanceledOrTimedOutRefreshDoesNotRenewCache(t *testing.T) {
	for _, timedOut := range []bool{false, true} {
		t.Run(strconv.FormatBool(timedOut), func(t *testing.T) {
			var blocked atomic.Bool
			g, clock := r17RouteGens(t, func(w http.ResponseWriter, r *http.Request) {
				if blocked.Load() {
					<-r.Context().Done()
					return
				}
				io.WriteString(w, `{"gens":{},"desktops":["abc-1"]}`)
			})
			if err := g.refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			blocked.Store(true)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if timedOut {
				g.client.Timeout = time.Millisecond
			} else {
				cancel()
			}
			if err := g.refresh(ctx); err == nil {
				t.Fatal("canceled/timed out refresh accepted")
			}
			if _, ok := g.gen(ctx, "abc-1"); !ok {
				t.Error("fresh table lost on canceled/timed out refresh")
			}
			clock.Add(int64(2*routeGenPoll + routeGenTimeout))
			if _, ok := g.gen(ctx, "abc-1"); ok {
				t.Error("expired table accepted after canceled/timed out refresh")
			}
			if g.hasDesktop(ctx, "abc-1") {
				t.Error("expired desktop table accepted after canceled/timed out refresh")
			}
		})
	}
}
