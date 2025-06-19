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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	port = flag.Int("port", 6255, "port to listen on (default 6255)")
	verbose = flag.Bool("v", false, "enable verbose (debug) logging")

	// dynamic gauge metric registry: sensor-name → GaugeVec
	gaugeMap = struct {
		sync.RWMutex
		m map[string]*prometheus.GaugeVec
	}{m: make(map[string]*prometheus.GaugeVec)}

	// Parameters we never turn into metrics
	// may or may not exist depending on your AW device
	ignoredKeys = []string {
		"PASSKEY",
		"MAC",
		"STATIONTYPE",
		"SOFTWARETYPE",
		"DATEUTC",
		"TZ",
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

	// AWP Devices don't properly split query params with a ?
	// and use another & instead...
	urlPath, queryParams, _ := strings.Cut(r.URL.Path, "&")

	// Grab the station this request is for from the path
	awpStation := path.Base(urlPath)
	slog.Debug("AWP data recieved", "station", awpStation, "remote", r.RemoteAddr)

	// Parse the query parameters
	vals, err := url.ParseQuery(queryParams)
	if err != nil {
		slog.Warn("Failed to parse some query parameters. Continuing with successfully parsed parameters...",
		slog.String("station", awpStation), slog.Any("error", err))
	}
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

func main() {
	flag.Parse()
	initLogger()

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
