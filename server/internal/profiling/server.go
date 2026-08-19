package profiling

import (
	"net/http"
	httppprof "net/http/pprof"
	"os"
	"runtime"
	"strings"
	"time"
)

type Config struct {
	Addr string
}

func ConfigFromEnv() Config {
	return Config{Addr: strings.TrimSpace(os.Getenv("PPROF_ADDR"))}
}

func (c Config) Enabled() bool {
	return strings.TrimSpace(c.Addr) != ""
}

func ConfigureRuntime(blockProfileRate, mutexProfileFraction int) {
	runtime.SetBlockProfileRate(blockProfileRate)
	runtime.SetMutexProfileFraction(mutexProfileFraction)
}

func NewHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /debug/pprof/", httppprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", httppprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", httppprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", httppprof.Symbol)
	mux.HandleFunc("POST /debug/pprof/symbol", httppprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", httppprof.Trace)
	return mux
}

func NewServer(addr string) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           NewHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
		// CPU profiles and traces run for a caller-selected duration. Leave
		// ReadTimeout and WriteTimeout unset so long captures are not truncated.
	}
}
