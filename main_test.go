package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMirrorIntegration tests that requests are properly mirrored to the configured endpoint
func TestMirrorIntegration(t *testing.T) {
	// Channel to synchronize the test
	mirrorReceived := make(chan bool, 1)
	var mirrorRequest *http.Request
	var mirrorMutex sync.Mutex

	// Create a mock mirror server that captures requests
	mirrorServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mirrorMutex.Lock()
		mirrorRequest = r
		mirrorMutex.Unlock()

		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))

		// Signal that we received the mirror request
		select {
		case mirrorReceived <- true:
		default:
		}
	}))
	defer mirrorServer.Close()

	// Parse the mirror server URL
	mirrorURL, err := url.Parse(mirrorServer.URL)
	if err != nil {
		t.Fatalf("Failed to parse mirror server URL: %v", err)
	}

	// Configure mirroring to point to our test server
	host := mirrorURL.Hostname()
	port, _ := strconv.Atoi(mirrorURL.Port())
	*mirrorHost = host
	*mirrorPort = port
	*mirrorPath = "/data/report"
	*mirrorHTTPS = false

	// Create the main AWP exporter server
	awpServer := httptest.NewServer(http.HandlerFunc(reportHandler))
	defer awpServer.Close()

	// Prepare test data - simulate an AWP weather station request
	testData := url.Values{
		"PASSKEY":        {"test-passkey"},
		"stationtype":    {"EasyWeatherV1.6.4"},
		"dateutc":        {"2025-01-10+18:30:00"},
		"tempf":          {"72.5"},
		"humidity":       {"45"},
		"windspeedmph":   {"5.2"},
		"windgustmph":    {"8.1"},
		"winddir":        {"270"},
		"baromin":        {"30.15"},
		"dailyrainin":    {"0.00"},
		"solarradiation": {"150"},
		"UV":             {"2"},
	}

	// Send the test request to the AWP exporter
	testURL := fmt.Sprintf("%s/data/report/TEST_STATION?%s", awpServer.URL, testData.Encode())
	resp, err := http.Get(testURL)
	if err != nil {
		t.Fatalf("Failed to send test request: %v", err)
	}
	defer resp.Body.Close()

	// Verify the AWP exporter responded correctly
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("Expected status %d, got %d", http.StatusNoContent, resp.StatusCode)
	}

	// Wait for the mirror request to be received (with timeout)
	select {
	case <-mirrorReceived:
		// Mirror request received successfully
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout waiting for mirror request")
	}

	// Verify the mirror request was received correctly
	mirrorMutex.Lock()
	defer mirrorMutex.Unlock()

	if mirrorRequest == nil {
		t.Fatal("Mirror request was not received")
	}

	// Verify the mirror request details
	if mirrorRequest.Method != "GET" {
		t.Errorf("Expected GET method, got %s", mirrorRequest.Method)
	}

	if !strings.HasSuffix(mirrorRequest.URL.Path, *mirrorPath) {
		t.Errorf("Expected path to end with /TEST_STATION, got %s", mirrorRequest.URL.Path)
	}

	// Verify query parameters were forwarded correctly
	mirrorQuery := mirrorRequest.URL.Query()

	// Check that sensor data was forwarded (non-ignored parameters)
	expectedParams := []string{"tempf", "humidity", "windspeedmph", "windgustmph", "winddir", "baromin", "dailyrainin", "solarradiation", "UV"}
	for _, param := range expectedParams {
		if !mirrorQuery.Has(param) {
			t.Errorf("Expected parameter %s to be forwarded to mirror", param)
		} else if mirrorQuery.Get(param) != testData.Get(param) {
			t.Errorf("Parameter %s: expected %s, got %s", param, testData.Get(param), mirrorQuery.Get(param))
		}
	}

	// Check that ignored parameters were also forwarded (they should be in the mirror but not in metrics)
	// Note: URL query parameters are case-sensitive, so we need to check the exact case
	ignoredParams := map[string]string{
		"PASSKEY":     "PASSKEY",
		"stationtype": "stationtype",
		"dateutc":     "dateutc",
	}
	for originalParam, expectedParam := range ignoredParams {
		if !mirrorQuery.Has(expectedParam) {
			t.Errorf("Expected ignored parameter %s to be forwarded to mirror as %s", originalParam, expectedParam)
		}
	}

	// Verify User-Agent header
	userAgent := mirrorRequest.Header.Get("User-Agent")
	if userAgent != "awp-exporter-mirror/1.0" {
		t.Errorf("Expected User-Agent 'awp-exporter-mirror/1.0', got '%s'", userAgent)
	}
}
