package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestR18InvalidRevocationSnapshotsHTTP(t *testing.T) {
	for name, invalid := range map[string]string{
		"missing": `{}`, "null": `null`, "wrong_endpoint": `{"status":"ok"}`, "null_gens": `{"gens":null,"desktops":[]}`,
		"array": `[]`, "wrong_gens": `{"gens":[]}`, "null_generation": `{"gens":{"abc-1":null}}`,
		"negative": `{"gens":{"abc-1":-1}}`, "fraction": `{"gens":{"abc-1":1.5}}`, "string": `{"gens":{"abc-1":"1"}}`,
		"overflow": `{"gens":{"abc-1":999999999999999999999999}}`, "wrong_desktops": `{"gens":{},"desktops":{}}`,
		"null_desktop": `{"gens":{},"desktops":[null]}`, "null_desktops": `{"gens":{},"desktops":null}`,
		"trailing": `{"gens":{}} garbage`, "second_document": `{"gens":{}} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			var mode atomic.Int32
			gens, clock := r17RouteGens(t, func(w http.ResponseWriter, r *http.Request) {
				if mode.Load() == 1 {
					io.WriteString(w, invalid)
					return
				}
				io.WriteString(w, `{"gens":{"abc-1":1},"desktops":["abc-1"]}`)
			})
			g, _, port := setup(t)
			g.gens, g.now = gens, gens.now
			server := httptest.NewServer(g)
			defer server.Close()
			if err := gens.refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			fetched, applied := gens.fetched, gens.applied
			read := func(desktop bool, gen, want int) {
				t.Helper()
				host := strconv.Itoa(port) + "-abc-1.vm.boatvms.test"
				token := first(tok(func(e int64) string { return signRoute(g.k(purposePortRoute), port, "abc-1", gen, e) }, time.Hour))
				name := routeCookie
				if desktop {
					host = "abc-1.desktop.boat.test"
					name = desktopCookie
					token = first(tok(func(e int64) string { return signDesktop(g.k(purposeDesktopLink), "abc-1", gen, e) }, time.Hour))
				}
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
					t.Errorf("desktop=%v gen=%d: status=%d body=%q want=%d", desktop, gen, resp.StatusCode, body, want)
				}
			}
			for _, desktop := range []bool{false, true} {
				read(desktop, 0, 401)
				read(desktop, 1, 200)
			}
			mode.Store(1)
			clock.Add(int64(routeGenMaxAge - time.Nanosecond))
			if err := gens.refresh(t.Context()); err == nil {
				t.Error("invalid snapshot accepted")
			}
			if !gens.fetched.Equal(fetched) || gens.applied != applied || gens.gens["abc-1"] != 1 || !gens.desktops["abc-1"] {
				t.Error("invalid snapshot mutated cache")
			}
			for _, desktop := range []bool{false, true} {
				read(desktop, 0, 401)
				read(desktop, 1, 200)
			}
			clock.Add(1)
			for _, desktop := range []bool{false, true} {
				read(desktop, 0, 503)
				read(desktop, 1, 503)
			}
			if gens.hasDesktop(t.Context(), "abc-1") {
				t.Error("invalid snapshot renewed desktop freshness")
			}
			mode.Store(0)
			for _, desktop := range []bool{false, true} {
				read(desktop, 0, 401)
				read(desktop, 1, 200)
			}
		})
	}
}

func TestR18RevocationSnapshotControls(t *testing.T) {
	for _, body := range []string{`{"gens":{}}`, `{"gens":{},"desktops":[]}`, `{"gens":{"abc-1":0,"other":2},"desktops":["abc-1"]}` + " \n"} {
		t.Run(body, func(t *testing.T) {
			g, _ := r17RouteGens(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) })
			if err := g.refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			if n, ok := g.gen(t.Context(), "abc-1"); !ok || n != 0 {
				t.Fatalf("generation=%d fresh=%v", n, ok)
			}
		})
	}
}

func TestR18OversizedRevocationSnapshot(t *testing.T) {
	g, _ := r17RouteGens(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"gens":{}}`)
		chunk := strings.Repeat(" ", 1<<20)
		for range 64 {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
	})
	g.client.Timeout = 10 * time.Second
	if err := g.refresh(t.Context()); err == nil {
		t.Error("oversized snapshot accepted")
	}
	if g.ready {
		t.Error("oversized snapshot initialized cache")
	}
}
