// Command server exposes a chosen cache implementation over a small HTTP JSON
// API. It exists for the optional end-to-end (Track B) experiment in the blog
// post: drive it with a load generator such as fortio, wrk2, or bombardier
// and observe that, behind HTTP and JSON, the implementations converge --
// because the net/http and encoding/json costs dwarf the synchronization
// differences that the in-process benchmarks (Track A) measure.
//
// This is NOT the primary measurement path. Use `go test -bench` for that.
//
// Usage:
//
//	go run ./cmd/server -impl=sharded -addr=:8080
//	curl -s 'localhost:8080/get?key=foo'
//	curl -s -XPOST localhost:8080/set -d '{"key":"foo","value":"bar"}'
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"

	cache "inmemcache"
)

type setRequest struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type getResponse struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Found bool   `json:"found"`
}

func main() {
	impl := flag.String("impl", "sharded", "cache implementation: naive|mutex|rwmutex|syncmap|sharded|cow")
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	c, err := cache.New(*impl)
	if err != nil {
		log.Fatalf("server: %v", err)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/get", func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		value, found := c.Get(key)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(getResponse{Key: key, Value: value, Found: found})
	})

	mux.HandleFunc("/set", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost && r.Method != http.MethodPut {
			http.Error(w, "use POST or PUT", http.StatusMethodNotAllowed)
			return
		}
		var req setRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		c.Set(req.Key, req.Value)
		w.WriteHeader(http.StatusNoContent)
	})

	log.Printf("serving %q cache on %s", *impl, *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
