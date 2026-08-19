package profiling

import (
	"net/http"
	httppprof "net/http/pprof"
	"time"
)

// Addr is deliberately fixed to loopback so runtime profiles cannot be
// exposed through the public API listener or a container's network interface.
const Addr = "127.0.0.1:6060"

func NewHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /debug/pprof/", httppprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", httppprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", httppprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", httppprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", httppprof.Trace)
	return mux
}

func NewServer() *http.Server {
	return &http.Server{
		Addr:              Addr,
		Handler:           NewHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
}
