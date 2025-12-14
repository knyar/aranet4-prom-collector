package promsync

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/castai/promwrite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew(t *testing.T) {
	tests := []struct {
		name    string
		config  Config
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid config",
			config: Config{
				PrometheusEndpoint: "http://localhost:9090",
				MetricPrefix:       "test_",
				Labels:             map[string]string{"job": "test"},
			},
			wantErr: false,
		},
		{
			name: "missing endpoint",
			config: Config{
				PrometheusEndpoint: "",
				MetricPrefix:       "test_",
			},
			wantErr: true,
			errMsg:  "PrometheusEndpoint is required",
		},
		{
			name: "invalid URL",
			config: Config{
				PrometheusEndpoint: "://invalid",
				MetricPrefix:       "test_",
			},
			wantErr: true,
			errMsg:  "failed to parse URL",
		},
		{
			name: "URL without host",
			config: Config{
				PrometheusEndpoint: "http://",
				MetricPrefix:       "test_",
			},
			wantErr: true,
			errMsg:  "has no host",
		},
		{
			name: "URL without scheme",
			config: Config{
				PrometheusEndpoint: "localhost:9090",
				MetricPrefix:       "test_",
			},
			wantErr: true,
			errMsg:  "has no host", // URL parser treats this as missing host
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			syncer, err := New(tt.config)
			if tt.wantErr {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.errMsg)
				require.Nil(t, syncer)
			} else {
				require.NoError(t, err)
				require.NotNil(t, syncer)
				assert.Equal(t, tt.config.MetricPrefix, syncer.config.MetricPrefix)
			}
		})
	}
}

func TestReportMetric_Validation(t *testing.T) {
	// Create a minimal test server
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := map[string]interface{}{
			"status": "success",
			"data": map[string]interface{}{
				"resultType": "vector",
				"result":     []interface{}{},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}))
	defer apiServer.Close()

	config := Config{
		PrometheusEndpoint: apiServer.URL,
		MetricPrefix:       "test_",
		Labels:             map[string]string{"job": "test"},
	}
	syncer, err := New(config)
	require.NoError(t, err)

	ctx := context.Background()
	now := time.Now()

	tests := []struct {
		name    string
		metric  string
		ts      time.Time
		value   float64
		wantErr bool
		errMsg  string
	}{
		{
			name:    "zero timestamp",
			metric:  "test_metric",
			ts:      time.Time{},
			value:   1.0,
			wantErr: true,
			errMsg:  "zero timestamp",
		},
		{
			name:    "future timestamp too far",
			metric:  "test_metric",
			ts:      now.Add(2 * time.Hour),
			value:   1.0,
			wantErr: true,
			errMsg:  "too far in the future",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := syncer.ReportMetric(ctx, tt.metric, tt.ts, tt.value)
			if tt.wantErr {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.errMsg)
			}
		})
	}
}

func TestReportMetric_ColdStart(t *testing.T) {
	// Mock API server that returns empty result (cold start)
	apiHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/query" {
			response := map[string]interface{}{
				"status": "success",
				"data": map[string]interface{}{
					"resultType": "vector",
					"result":     []interface{}{}, // Empty result = cold start
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(response)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	})

	writeCount := 0
	writeHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/write" {
			writeCount++
			w.WriteHeader(http.StatusNoContent)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	})

	syncer := createTestSyncerWithMocks(t, apiHandler, writeHandler)

	ctx := context.Background()
	now := time.Now()

	// First metric should be written (cold start - no existing data)
	err := syncer.ReportMetric(ctx, "test_metric", now, 42.0)
	require.NoError(t, err)

	// Verify write was called
	assert.Equal(t, 1, writeCount, "Write should be called once on cold start")
}

func TestReportMetric_DuplicateDetection(t *testing.T) {
	// Mock API server that returns a previous timestamp
	previousTime := time.Now().Add(-1 * time.Hour)
	previousTimestamp := float64(previousTime.Unix())

	apiHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/query" {
			response := map[string]interface{}{
				"status": "success",
				"data": map[string]interface{}{
					"resultType": "vector",
					"result": []interface{}{
						map[string]interface{}{
							"metric": map[string]interface{}{},
							"value": []interface{}{
								previousTimestamp,
								fmt.Sprintf("%.0f", previousTimestamp),
							},
						},
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(response)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	})

	writeCount := 0
	writeHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/write" {
			writeCount++
			w.WriteHeader(http.StatusNoContent)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	})

	syncer := createTestSyncerWithMocks(t, apiHandler, writeHandler)

	ctx := context.Background()
	now := time.Now()

	// First report with timestamp after previous - should write
	err := syncer.ReportMetric(ctx, "test_metric", now, 42.0)
	require.NoError(t, err)
	assert.Equal(t, 1, writeCount, "Should write new metric")

	// Second report with same timestamp - should be skipped (using cached lastTime)
	err = syncer.ReportMetric(ctx, "test_metric", now, 43.0)
	require.NoError(t, err)
	assert.Equal(t, 1, writeCount, "Should skip duplicate timestamp")

	// Third report with earlier timestamp - should be skipped
	err = syncer.ReportMetric(ctx, "test_metric", previousTime, 44.0)
	require.NoError(t, err)
	assert.Equal(t, 1, writeCount, "Should skip older timestamp")

	// Fourth report with later timestamp - should write
	futureTime := now.Add(1 * time.Minute)
	err = syncer.ReportMetric(ctx, "test_metric", futureTime, 45.0)
	require.NoError(t, err)
	assert.Equal(t, 2, writeCount, "Should write newer timestamp")
}

func TestReportMetric_DryRun(t *testing.T) {
	apiHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/query" {
			response := map[string]interface{}{
				"status": "success",
				"data": map[string]interface{}{
					"resultType": "vector",
					"result":     []interface{}{},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(response)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	})

	writeCount := 0
	writeHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeCount++
		w.WriteHeader(http.StatusNoContent)
	})

	apiServer := httptest.NewServer(apiHandler)
	defer apiServer.Close()

	writeServer := httptest.NewServer(writeHandler)
	defer writeServer.Close()

	config := Config{
		PrometheusEndpoint: apiServer.URL,
		MetricPrefix:       "test_",
		Labels:             map[string]string{"job": "test"},
		DryRun:             true, // Enable dry run
	}

	syncer, err := New(config)
	require.NoError(t, err)
	syncer.write = promwrite.NewClient(writeServer.URL + "/api/v1/write")

	ctx := context.Background()
	now := time.Now()

	// In dry run mode, should not actually write
	err = syncer.ReportMetric(ctx, "test_metric", now, 42.0)
	require.NoError(t, err)
	assert.Equal(t, 0, writeCount, "Dry run should not call write")
}

func TestReportMetric_ErrorHandling(t *testing.T) {
	tests := []struct {
		name         string
		apiHandler   http.HandlerFunc
		writeHandler http.HandlerFunc
		expectError  bool
		errMsg       string
	}{
		{
			name: "API query error",
			apiHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			writeHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			},
			expectError: true,
			errMsg:      "querying metric",
		},
		{
			name: "API returns non-vector",
			apiHandler: func(w http.ResponseWriter, r *http.Request) {
				response := map[string]interface{}{
					"status": "success",
					"data": map[string]interface{}{
						"resultType": "scalar", // Wrong type
						"result":     []interface{}{},
					},
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(response)
			},
			writeHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			},
			expectError: true,
			errMsg:      "querying metric", // The actual error is in parsing, but we check for query error
		},
		{
			name: "API returns multiple time series",
			apiHandler: func(w http.ResponseWriter, r *http.Request) {
				response := map[string]interface{}{
					"status": "success",
					"data": map[string]interface{}{
						"resultType": "vector",
						"result": []interface{}{
							map[string]interface{}{"metric": map[string]interface{}{}},
							map[string]interface{}{"metric": map[string]interface{}{}}, // Two results
						},
					},
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(response)
			},
			writeHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			},
			expectError: true,
			errMsg:      "multiple time series",
		},
		{
			name: "Write error",
			apiHandler: func(w http.ResponseWriter, r *http.Request) {
				response := map[string]interface{}{
					"status": "success",
					"data": map[string]interface{}{
						"resultType": "vector",
						"result":     []interface{}{},
					},
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(response)
			},
			writeHandler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			expectError: true,
			errMsg:      "sending request",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			syncer := createTestSyncerWithMocks(t, tt.apiHandler, tt.writeHandler)

			ctx := context.Background()
			now := time.Now()

			err := syncer.ReportMetric(ctx, "test_metric", now, 42.0)
			if tt.expectError {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.errMsg)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestLabelSet(t *testing.T) {
	config := Config{
		MetricPrefix: "test_",
		Labels: map[string]string{
			"job":      "test-job",
			"instance": "test-instance",
			"device":   "test-device",
		},
	}

	syncer := &Syncer{config: &config}
	labels := syncer.labelSet("my_metric")

	// Check __name__ label
	require.Equal(t, "test_my_metric", labels.Get("__name__"))

	// Check additional labels
	require.Equal(t, "test-job", labels.Get("job"))
	require.Equal(t, "test-instance", labels.Get("instance"))
	require.Equal(t, "test-device", labels.Get("device"))

	// Verify labels are sorted
	labelNames := make([]string, len(labels))
	for i, l := range labels {
		labelNames[i] = l.Name
	}
	// Check that labels are in sorted order
	for i := 1; i < len(labelNames); i++ {
		require.True(t, labelNames[i-1] < labelNames[i], "Labels should be sorted")
	}
}

// createTestSyncerWithMocks creates a syncer with mocked dependencies for testing
func createTestSyncerWithMocks(t *testing.T, apiHandler http.HandlerFunc, writeHandler http.HandlerFunc) *Syncer {
	apiServer := httptest.NewServer(apiHandler)
	writeServer := httptest.NewServer(writeHandler)
	t.Cleanup(func() {
		apiServer.Close()
		writeServer.Close()
	})

	config := Config{
		PrometheusEndpoint: apiServer.URL,
		MetricPrefix:       "test_",
		Labels:             map[string]string{"job": "test", "instance": "test-instance"},
		DryRun:             false,
	}

	syncer, err := New(config)
	require.NoError(t, err)

	// Replace the write client with one pointing to our test server
	writeURL := writeServer.URL + "/api/v1/write"
	syncer.write = promwrite.NewClient(writeURL)

	return syncer
}
