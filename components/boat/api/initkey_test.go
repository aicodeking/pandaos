package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// Council round 3: admin init printed only JSON and the owner pasted all of it
// as the key. On a terminal it now prints the bare token on its own line.
func TestInitKeyOutput(t *testing.T) {
	k := keyView{ID: "k1", Name: "initial", Token: "boat_k1_secret"}
	var text bytes.Buffer
	printInitKey(&text, k, false)
	lines := strings.Split(text.String(), "\n")
	found := false
	for i, l := range lines {
		if l == k.Token && i > 0 && strings.Contains(strings.ToLower(lines[i-1]), "key") {
			found = true
		}
	}
	if !found || strings.Contains(text.String(), "{") {
		t.Errorf("text output has no bare, labelled token line:\n%s", text.String())
	}
	var js bytes.Buffer
	printInitKey(&js, k, true)
	var got keyView
	if err := json.Unmarshal(js.Bytes(), &got); err != nil || got.Token != k.Token {
		t.Errorf("json output: %v %q", err, js.String())
	}
}
