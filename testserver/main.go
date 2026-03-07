package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

const version = "v1.0.1"

var requestCount atomic.Int64
var startTime = time.Now()

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleRoot)
	mux.HandleFunc("/health", handleHealth)
	mux.HandleFunc("/slow", handleSlow)

	addr := ":" + port
	log.Printf("testserver %s listening on http://localhost%s", version, addr)

	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func handleRoot(w http.ResponseWriter, r *http.Request) {
	n := requestCount.Add(1)
	fmt.Fprintf(w, "Hello from testserver %s\n", version)
	fmt.Fprintf(w, "Uptime:   %s\n", time.Since(startTime).Round(time.Second))
	fmt.Fprintf(w, "Requests: %d\n", n)
	fmt.Fprintf(w, "Time:     %s\n", time.Now().Format(time.RFC3339))
	fmt.Fprintf(w, "Path:     %s %s\n", r.Method, r.URL.Path)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"version": version,
		"uptime":  time.Since(startTime).Round(time.Second).String(),
	})
}

func handleSlow(w http.ResponseWriter, r *http.Request) {
	log.Printf("slow request started — sleeping 3s")
	time.Sleep(3 * time.Second)
	fmt.Fprintf(w, "slow response from %s\n", version)
}
