// watchtower-ui serves the embedded web UI and reverse-proxies /api to the
// manager API (D-001: the UI never talks to the cluster directly;
// D-008: REST + WS over a single origin).
package main

import (
	"flag"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"

	"github.com/krbck/watchtower/ui"
)

func main() {
	listen := flag.String("listen", envOr("WATCHTOWER_UI_LISTEN", ":8080"), "UI listen address")
	apiURL := flag.String("api", envOr("WATCHTOWER_API_URL", "http://localhost:8482"), "watchtower manager API URL")
	flag.Parse()

	target, err := url.Parse(*apiURL)
	if err != nil {
		log.Fatalf("invalid api url %q: %v", *apiURL, err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// REST + WebSocket both live under /api (ReverseProxy upgrades WS).
	mux.Handle("/api/", proxy)
	mux.Handle("/", http.FileServer(http.FS(ui.FS)))

	log.Printf("watchtower-ui listening on %s (api: %s)", *listen, *apiURL)
	log.Fatal(http.ListenAndServe(*listen, mux))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
