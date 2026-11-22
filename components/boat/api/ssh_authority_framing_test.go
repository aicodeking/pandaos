package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSSHAuthorityGuestResponseFraming(t *testing.T) {
	e := setup(t)
	_, vm := e.jumpSetup()
	small := strings.Join(strings.Fields(r11Pub)[:2], " ")
	// A deterministic 16384-bit RSA public blob avoids slow private-key generation.
	modulus := bytes.Repeat([]byte{0xa5}, 2049)
	modulus[0], modulus[1] = 0, 0x80
	var blob []byte
	for _, part := range [][]byte{[]byte("ssh-rsa"), {1, 0, 1}, modulus} {
		blob = binary.BigEndian.AppendUint32(blob, uint32(len(part)))
		blob = append(blob, part...)
	}
	large := "ssh-rsa " + base64.StdEncoding.EncodeToString(blob)
	for _, pub := range []string{small, large} {
		if code, out := e.do(e.owner, "POST", vmPath(vm, "ssh"), map[string]any{"public_key": pub}); code != 200 {
			t.Fatalf("grant: %d %v", code, out)
		}
	}
	unrelatedBlob, err := base64.StdEncoding.DecodeString(strings.Fields(small)[1])
	if err != nil {
		t.Fatal(err)
	}
	unrelatedBlob[len(unrelatedBlob)-1] ^= 1
	unrelated := "ssh-ed25519 " + base64.StdEncoding.EncodeToString(unrelatedBlob)
	e.boatd.mu.Lock()
	configBytes := bytes.Clone(e.boatd.files[vm+":/etc/boat/ssh-auth.json"])
	e.boatd.mu.Unlock()
	var config struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(configBytes, &config); err != nil {
		t.Fatal(err)
	}
	httpAPI := httptest.NewServer(e.srv)
	defer httpAPI.Close()
	tlsAPI := httptest.NewTLSServer(e.srv)
	defer tlsAPI.Close()
	for _, transport := range []struct {
		name string
		api  *httptest.Server
	}{{"http", httpAPI}, {"tls", tlsAPI}} {
		t.Run(transport.name, func(t *testing.T) {
			for _, tc := range []struct {
				name string
				pub  string
				code int
			}{{"small", small, 200}, {"large", large, 200}, {"unrelated", unrelated, 204}} {
				t.Run(tc.name, func(t *testing.T) {
					fields := strings.Fields(tc.pub)
					body, err := json.Marshal(sshLookupRequest{User: "dev", KeyType: fields[0], KeyBlob: fields[1]})
					if err != nil {
						t.Fatal(err)
					}
					dialer := &net.Dialer{Timeout: 5 * time.Second}
					var conn net.Conn
					if transport.api.TLS == nil {
						conn, err = dialer.Dial("tcp", transport.api.Listener.Addr().String())
					} else {
						tlsConfig := transport.api.Client().Transport.(*http.Transport).TLSClientConfig
						conn, err = tls.DialWithDialer(dialer, "tcp", transport.api.Listener.Addr().String(), tlsConfig)
					}
					if err != nil {
						t.Fatal(err)
					}
					defer conn.Close()
					if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
						t.Fatal(err)
					}
					if _, err := fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", vmPath(vm, "ssh", "authorize"), config.Token, len(body), body); err != nil {
						t.Fatal(err)
					}
					// Read raw headers and body: http.Client normalizes duplicate lengths and dechunks.
					reader := textproto.NewReader(bufio.NewReader(conn))
					status, err := reader.ReadLine()
					if err != nil {
						t.Fatal(err)
					}
					headers, err := reader.ReadMIMEHeader()
					if err != nil {
						t.Fatal(err)
					}
					responseBody, err := io.ReadAll(reader.R)
					if err != nil {
						t.Fatalf("response did not reach EOF: %v", err)
					}
					if want := fmt.Sprintf("HTTP/1.1 %d %s", tc.code, http.StatusText(tc.code)); status != want {
						t.Fatalf("status=%q want %q", status, want)
					}
					if headers.Get("Cache-Control") != "no-store" {
						t.Error("authorization response must remain non-cacheable")
					}
					if values := headers.Values("Transfer-Encoding"); len(values) != 0 {
						t.Errorf("Transfer-Encoding=%q want none", values)
					}
					if tc.code == 204 {
						if len(responseBody) != 0 {
							t.Fatalf("denied key received %d body bytes", len(responseBody))
						}
						return
					}
					wantBody, err := json.Marshal(map[string]string{"authorized_key": tc.pub})
					if err != nil {
						t.Fatal(err)
					}
					wantBody = append(wantBody, '\n')
					if tc.name == "large" && len(wantBody) <= 2048 {
						t.Fatal("large-key response must exceed Go's automatic Content-Length buffer")
					}
					if lengths := headers.Values("Content-Length"); len(lengths) != 1 || lengths[0] != strconv.Itoa(len(wantBody)) {
						t.Errorf("Content-Length=%q want exactly [%d]", lengths, len(wantBody))
					}
					if headers.Get("Content-Type") != "application/json" {
						t.Errorf("Content-Type=%q", headers.Get("Content-Type"))
					}
					if !bytes.Equal(responseBody, wantBody) {
						t.Errorf("body differs from complete authorized-key JSON plus newline: got %d bytes, want %d", len(responseBody), len(wantBody))
					}
				})
			}
		})
	}
}
