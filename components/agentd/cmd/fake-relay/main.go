// fake-relay is an in-memory stand-in for the PandaConnect relay, for tests.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"pandaos/agentd/internal/fakerelay"
)

func main() {
	listen := flag.String("listen", ":8787", "listen address")
	enqueue := flag.Bool("enqueue", true, "serve admin endpoints POST /admin/jobs and GET /admin/results/{id}")
	flag.Parse()
	log.Printf("fake-relay listening on %s (admin=%v)", *listen, *enqueue)
	log.Fatal(http.ListenAndServe(*listen, fakerelay.New(30*time.Second).Handler(*enqueue)))
}
