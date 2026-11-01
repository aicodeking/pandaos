package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// OpenTelemetry over OTLP/HTTP JSON, stdlib only. The instance exporter
// (-otlp-endpoint) gets request spans and VM lifecycle logs. A team's own
// endpoint (PUT /v1/telemetry) gets that team's lifecycle logs only: never spans
// of the API, prompts, or command output. Export never blocks a request; when
// the queue is full, data is dropped and counted.

const (
	otelBatch   = 256
	otelQueue   = 8192
	otelEvery   = 5 * time.Second
	otelService = "boat-api"
)

type otelExporter struct {
	endpoint string
	headers  map[string]string
	client   *http.Client
	spans    chan map[string]any
	logs     chan map[string]any
	mu       sync.Mutex
	dropped  int64
}

func newOTel(endpoint string, headers map[string]string) *otelExporter {
	return &otelExporter{endpoint: strings.TrimRight(endpoint, "/"), headers: headers,
		client: &http.Client{Timeout: 15 * time.Second},
		spans:  make(chan map[string]any, otelQueue), logs: make(chan map[string]any, otelQueue)}
}

func otelAttrs(kv map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(kv))
	for k, v := range kv {
		switch t := v.(type) {
		case string:
			if t != "" {
				out = append(out, map[string]any{"key": k, "value": map[string]any{"stringValue": t}})
			}
		case int:
			out = append(out, map[string]any{"key": k, "value": map[string]any{"intValue": strconv.Itoa(t)}})
		}
	}
	return out
}

func otelResource() map[string]any {
	return map[string]any{"attributes": otelAttrs(map[string]any{"service.name": otelService})}
}

func unixNano(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }

func (o *otelExporter) enqueue(c chan map[string]any, v map[string]any) {
	select {
	case c <- v:
	default:
		o.mu.Lock()
		o.dropped++
		o.mu.Unlock()
	}
}

func (o *otelExporter) span(traceID, parent, name string, start, end time.Time, status int, attrs map[string]any) {
	if o == nil {
		return
	}
	code := 1
	if status >= 500 {
		code = 2
	}
	sp := map[string]any{"traceId": traceID, "spanId": randHex(8), "name": name, "kind": 2,
		"startTimeUnixNano": unixNano(start), "endTimeUnixNano": unixNano(end),
		"attributes": otelAttrs(attrs), "status": map[string]any{"code": code}}
	if parent != "" {
		sp["parentSpanId"] = parent
	}
	o.enqueue(o.spans, sp)
}

func (o *otelExporter) event(rec map[string]any) {
	if o != nil {
		o.enqueue(o.logs, rec)
	}
}

func otelLogRecord(typ string, at time.Time, attrs map[string]any) map[string]any {
	attrs["event.name"] = typ
	return map[string]any{"timeUnixNano": unixNano(at), "observedTimeUnixNano": unixNano(at), "severityNumber": 9,
		"severityText": "INFO", "body": map[string]any{"stringValue": typ}, "attributes": otelAttrs(attrs)}
}

func logsPayload(recs []map[string]any) map[string]any {
	return map[string]any{"resourceLogs": []any{map[string]any{"resource": otelResource(),
		"scopeLogs": []any{map[string]any{"scope": map[string]any{"name": otelService}, "logRecords": recs}}}}}
}

func spansPayload(spans []map[string]any) map[string]any {
	return map[string]any{"resourceSpans": []any{map[string]any{"resource": otelResource(),
		"scopeSpans": []any{map[string]any{"scope": map[string]any{"name": otelService}, "spans": spans}}}}}
}

func otlpPost(ctx context.Context, client *http.Client, url string, headers map[string]string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// run batches and sends until ctx ends.
func (o *otelExporter) run(ctx context.Context) {
	if o == nil {
		return
	}
	t := time.NewTicker(otelEvery)
	defer t.Stop()
	var spans, logs []map[string]any
	flush := func() {
		sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if len(spans) > 0 {
			if err := otlpPost(sctx, o.client, o.endpoint+"/v1/traces", o.headers, spansPayload(spans)); err != nil {
				log.Printf("otlp traces: %v", err)
			}
			spans = nil
		}
		if len(logs) > 0 {
			if err := otlpPost(sctx, o.client, o.endpoint+"/v1/logs", o.headers, logsPayload(logs)); err != nil {
				log.Printf("otlp logs: %v", err)
			}
			logs = nil
		}
	}
	for {
		select {
		case <-ctx.Done():
			flush()
			return
		case sp := <-o.spans:
			if spans = append(spans, sp); len(spans) >= otelBatch {
				flush()
			}
		case rec := <-o.logs:
			if logs = append(logs, rec); len(logs) >= otelBatch {
				flush()
			}
		case <-t.C:
			flush()
		}
	}
}

var reTraceparent = regexp.MustCompile(`^00-([0-9a-f]{32})-([0-9a-f]{16})-[0-9a-f]{2}$`)

// traceIDs continues an incoming W3C trace, or starts one.
func traceIDs(header string) (traceID, parent string) {
	if m := reTraceparent.FindStringSubmatch(header); m != nil && m[1] != strings.Repeat("0", 32) {
		return m[1], m[2]
	}
	return randHex(16), ""
}

// emitVMEvent sends a lifecycle event to the instance and team exporters.
func (s *server) emitVMEvent(ctx context.Context, typ, team, vmID, state string) {
	attrs := func() map[string]any {
		return map[string]any{"boat.team_id": team, "boat.vm_id": vmID, "boat.vm_state": state}
	}
	now := time.Now()
	s.otel.event(otelLogRecord(typ, now, attrs()))
	tt := s.store.Snapshot().Telemetry[team]
	if tt == nil {
		return
	}
	headers, err := s.teamOTelHeaders(team, tt)
	if err != nil {
		log.Printf("team %s telemetry: %v", team, err)
		return
	}
	go func() {
		sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		// The webhook client refuses private targets (SSRF) and redirects.
		err := otlpPost(sctx, s.hooks.client, tt.Endpoint+"/v1/logs", headers, logsPayload([]map[string]any{otelLogRecord(typ, now, attrs())}))
		if err != nil {
			log.Printf("team %s telemetry: %v", team, err)
		}
	}()
}

func (s *server) teamOTelHeaders(team string, tt *TeamTelemetry) (map[string]string, error) {
	h := map[string]string{}
	if tt.SealedHeaders == "" {
		return h, nil
	}
	b, err := s.keys.Open(tt.SealedHeaders, "telemetry/"+team)
	if err != nil {
		return nil, err
	}
	return h, json.Unmarshal(b, &h)
}

// ---- team API ----

func telemetryView(tt *TeamTelemetry) map[string]any {
	if tt == nil {
		return map[string]any{"configured": false}
	}
	names := tt.HeaderNames
	if names == nil {
		names = []string{}
	}
	return map[string]any{"configured": true, "endpoint": tt.Endpoint, "header_names": names, "updated": tt.Updated}
}

func (s *server) getTelemetry(w http.ResponseWriter, r *http.Request, p *principal) {
	writeJSON(w, 200, telemetryView(s.store.Snapshot().Telemetry[p.Team.ID]))
}

var reHeaderName = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

func (s *server) putTelemetry(w http.ResponseWriter, r *http.Request, p *principal) {
	var body struct {
		Endpoint string            `json:"endpoint"`
		Headers  map[string]string `json:"headers"`
	}
	if !decode(w, r, &body) {
		return
	}
	endpoint := strings.TrimRight(body.Endpoint, "/")
	for _, sfx := range []string{"/v1/logs", "/v1/metrics", "/v1/traces"} {
		endpoint = strings.TrimSuffix(endpoint, sfx)
	}
	if !s.validHookURL(endpoint) {
		writeErr(w, r, 400, "invalid_request", "endpoint must be an absolute https URL")
		return
	}
	names := []string{}
	for k, v := range body.Headers {
		if !reHeaderName.MatchString(k) || strings.ContainsAny(v, "\r\n") {
			writeErr(w, r, 400, "invalid_request", "invalid header "+k)
			return
		}
		names = append(names, k)
	}
	tt := &TeamTelemetry{Endpoint: endpoint, HeaderNames: names, Updated: time.Now().UTC()}
	if len(body.Headers) > 0 {
		b, _ := json.Marshal(body.Headers)
		sealed, err := s.keys.Seal(b, "telemetry/"+p.Team.ID)
		if err != nil {
			writeErr(w, r, 500, "crypto_error", "could not encrypt")
			return
		}
		tt.SealedHeaders = sealed
	}
	if err := s.store.Update(func(st *State) error { st.Telemetry[p.Team.ID] = tt; return nil }); err != nil {
		s.storeErr(w, r, err)
		return
	}
	writeJSON(w, 200, telemetryView(tt))
}

func (s *server) deleteTelemetry(w http.ResponseWriter, r *http.Request, p *principal) {
	if err := s.store.Update(func(st *State) error { delete(st.Telemetry, p.Team.ID); return nil }); err != nil {
		s.storeErr(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
