package main

import (
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	port        = flag.Int("port", 6255, "port to listen on (default 6255)")
	verbose     = flag.Bool("v", false, "enable verbose (debug) logging")
	mirrorHost  = flag.String("mirror-host", "", "hostname to mirror requests to (enables mirroring when set)")
	mirrorPort  = flag.Int("mirror-port", 8000, "port to mirror requests to (default 8000)")
	mirrorPath  = flag.String("mirror-path", "/data/report", "path to mirror requests to (default /data/report)")
	mirrorHTTPS = flag.Bool("mirror-https", false, "use HTTPS for mirror requests (default false)")

	// dynamic gauge metric registry: sensor-name → GaugeVec
	gaugeMap = struct {
		sync.RWMutex
		m map[string]*prometheus.GaugeVec
	}{m: make(map[string]*prometheus.GaugeVec)}

	// Parameters we never turn into metrics
	// may or may not exist depending on your AW device
	ignoredKeys = []string{
		"PASSKEY",
		"MAC",
		"STATIONTYPE",
		"SOFTWARETYPE",
		"DATEUTC",
		"TZ",
	}

	// HTTP client for mirroring requests
	mirrorClient = &http.Client{
		Timeout: 10 * time.Second,
	}
)

func init() {
	// Disable default go collectors. This metric collector has a tiny resource footprint
	// that does not require advanced monitoring directly. Removing these collectors will reduce
	// the speed that Prometheus data directory grows without requiring users to filter out
	// base metrics.
	prometheus.Unregister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	prometheus.Unregister(collectors.NewGoCollector())
}

func getGauge(field string) *prometheus.GaugeVec {
	// Attempt to find an existing GaugeVec
	gaugeMap.Lock()
	defer gaugeMap.Unlock()
	g, ok := gaugeMap.m[field]
	if ok {
		return g
	}

	// An existing GaugeVec does not exist, create one
	g = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: fmt.Sprintf("awp_%s", field),
			Help: fmt.Sprintf("AWP sensor value for %s", field),
		},
		[]string{"station"},
	)
	prometheus.MustRegister(g)
	gaugeMap.m[field] = g

	slog.Debug("created GaugeVec", slog.String("metric", fmt.Sprintf("awp_%s", field)))
	return g
}

func reportHandler(w http.ResponseWriter, r *http.Request) {
	// Tiny response back to the station
	w.WriteHeader(http.StatusNoContent)

	var vals url.Values
	var awpStation string
	var err error

	// First, try to parse query parameters the proper way
	if r.URL.RawQuery != "" {
		// Standard query parameters exist, parse them normally
		vals, err = url.ParseQuery(r.URL.RawQuery)
		awpStation = path.Base(r.URL.Path)
	} else {
		// Not all AWP Devices properly split query params with a ?
		// and use another & instead...
		urlPath, queryParams, _ := strings.Cut(r.URL.Path, "&")
		awpStation = path.Base(urlPath)
		vals, err = url.ParseQuery(queryParams)
	}

	slog.Debug("AWP data received", "station", awpStation, "remote", r.RemoteAddr)
	if err != nil {
		slog.Warn("Failed to parse some query parameters. Continuing with successfully parsed parameters...",
			slog.String("station", awpStation), slog.Any("error", err))
	}

	// Mirror the request asynchronously (don't block processing)
	go mirrorRequest(r.URL, vals, awpStation)

	for k, vv := range vals {
		// Ignore any junk parameters
		if slices.Contains(ignoredKeys, strings.ToUpper(k)) {
			continue
		}

		// All remaining values should be parseable into float
		if f, err := strconv.ParseFloat(vv[0], 64); err == nil {
			getGauge(k).WithLabelValues(awpStation).Set(f)
		}
	}
}

// mirrorRequest forwards the request to the configured mirror endpoint
func mirrorRequest(originalURL *url.URL, vals url.Values, station string) {
	if *mirrorHost == "" {
		return // Mirroring disabled
	}

	scheme := "http"
	port := *mirrorPort
	if *mirrorHTTPS {
		scheme = "https"
		// If port is still the default 80, change it to 443 for HTTPS
		if port == 80 {
			port = 443
		}
	}

	// Build the mirror URL
	mirrorURL := &url.URL{
		Scheme:   scheme,
		Host:     fmt.Sprintf("%s:%d", *mirrorHost, port),
		Path:     strings.TrimSuffix(*mirrorPath, "/"),
		RawQuery: vals.Encode(),
	}

	// Create and send the mirror request
	req, err := http.NewRequest("GET", mirrorURL.String(), nil)
	if err != nil {
		slog.Warn("Failed to create mirror request",
			slog.String("station", station),
			slog.String("url", mirrorURL.String()),
			slog.Any("error", err))
		return
	}

	// Copy relevant headers from the original request
	req.Header.Set("User-Agent", "awp-exporter-mirror/1.0")

	resp, err := mirrorClient.Do(req)
	if err != nil {
		slog.Warn("Failed to send mirror request",
			slog.String("station", station),
			slog.String("url", mirrorURL.String()),
			slog.Any("error", err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		slog.Warn("Mirror request returned error status",
			slog.String("station", station),
			slog.String("url", mirrorURL.String()),
			slog.Int("status", resp.StatusCode))
	} else {
		slog.Debug("Mirror request successful",
			slog.String("station", station),
			slog.String("url", mirrorURL.String()),
			slog.Int("status", resp.StatusCode))
	}
}

func main() {
	flag.Parse()
	initLogger()

	// Log mirror configuration
	if *mirrorHost != "" {
		scheme := "http"
		if *mirrorHTTPS {
			scheme = "https"
		}
		slog.Info("Mirroring enabled",
			slog.String("host", *mirrorHost),
			slog.Int("port", *mirrorPort),
			slog.String("path", *mirrorPath),
			slog.String("scheme", scheme))
	} else {
		slog.Info("Mirroring disabled")
	}

	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/data/report/", reportHandler)

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("awp-exporter listening on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("server exited: %v", err)
	}
}

func initLogger() {
	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}

	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(handler))
}
