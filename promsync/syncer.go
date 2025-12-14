package promsync

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/castai/promwrite"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/prompb"

	"github.com/prometheus/client_golang/api"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Config holds configuration for the Prometheus syncer.
type Config struct {
	// PrometheusEndpoint is the base URL of the Prometheus instance (e.g., "http://localhost:9090/")
	PrometheusEndpoint string

	// MetricPrefix is the prefix to use for all metric names (e.g., "aranet4_")
	MetricPrefix string

	// Labels are additional labels to add to all metrics.
	// Common labels include "job", "instance", etc.
	Labels map[string]string

	// DryRun, if true, will log metrics instead of writing them to Prometheus.
	DryRun bool
}

// Syncer writes metrics to Prometheus using Remote Write API, attempting to avoid
// writing duplicate data by keeping track of the last reported time for each metric.
type Syncer struct {
	write  *promwrite.Client
	api    api.Client
	config *Config

	// metricWrites is a counter of metric write attempts.
	metricWrites *prometheus.CounterVec

	// lastTimes is a map of metric name to the last time it was written.
	lastTimes map[string]time.Time
}

// New creates a new Prometheus syncer with the given configuration.
func New(config Config) (*Syncer, error) {
	if config.PrometheusEndpoint == "" {
		return nil, fmt.Errorf("PrometheusEndpoint is required")
	}

	url, err := url.Parse(config.PrometheusEndpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to parse URL %q: %w", config.PrometheusEndpoint, err)
	}
	if url.Host == "" {
		return nil, fmt.Errorf("URL %q has no host", config.PrometheusEndpoint)
	}
	if url.Scheme == "" {
		return nil, fmt.Errorf("URL %q has no scheme", config.PrometheusEndpoint)
	}

	client, err := api.NewClient(api.Config{Address: url.String()})
	if err != nil {
		return nil, fmt.Errorf("failed to create Prometheus client: %w", err)
	}

	writeURL := url.JoinPath("/api/v1/write")
	slog.Debug("Prometheus syncer created", "write-url", writeURL.String(), "prefix", config.MetricPrefix, "labels", config.Labels)

	return &Syncer{
		write:     promwrite.NewClient(writeURL.String()),
		api:       client,
		config:    &config,
		lastTimes: make(map[string]time.Time),

		metricWrites: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: config.MetricPrefix + "prometheus_writes_total",
			Help: "Total number of metric write attempts by status",
		}, []string{"status"}),
	}, nil
}

// lastTime returns the last time a metric was reported.
func (s *Syncer) lastTime(ctx context.Context, metric string) (time.Time, error) {
	last, ok := s.lastTimes[metric]
	if ok {
		return last, nil
	}

	api := v1.NewAPI(s.api)
	query := fmt.Sprintf("timestamp(%s)", s.labelSet(metric).String())
	// aranet4 stores data locally for up to 30 days.
	// https://forum.aranet.com/aranet-home-devices-aranet4-aranet2-aranet-radiation-aranet-radon/how-long-does-the-aranet4-device-store-historic-data/
	v, warn, err := api.Query(ctx, query, time.Now(), v1.WithLookbackDelta(30*24*time.Hour))
	if err != nil {
		return time.Time{}, fmt.Errorf("querying metric %q: %w", metric, err)
	}
	if warn != nil {
		slog.Warn("warning querying metric", "metric", metric, "query", query, "warn", warn)
	}
	if v == nil {
		return time.Time{}, fmt.Errorf("no value returned for query %s", query)
	}
	slog.Debug("query result", "query", query, "value_type", v.Type(), "value", v)
	if v.Type() != model.ValVector {
		return time.Time{}, fmt.Errorf("query %s returned non-vector value", metric)
	}
	vec := v.(model.Vector)
	if len(vec) == 0 {
		slog.Warn("no time series matched query", "query", query)
		return time.Time{}, nil
	}
	if len(vec) > 1 {
		return time.Time{}, fmt.Errorf("multiple time series matched query %s: %+v", query, vec)
	}
	ts := float64(vec[0].Value)
	last = time.Unix(int64(ts), int64(ts*1000000000)%1000000000)
	slog.Debug("last time", "metric", metric, "value", ts, "last", last)
	s.lastTimes[metric] = last
	return last, nil
}

// ReportMetric writes a metric to Prometheus.
func (s *Syncer) ReportMetric(ctx context.Context, name string, ts time.Time, value float64) error {
	if ts.IsZero() {
		s.metricWrites.WithLabelValues("error").Inc()
		return fmt.Errorf("cannot report metric %q with zero timestamp", name)
	}
	now := time.Now()
	if ts.After(now.Add(time.Hour)) {
		s.metricWrites.WithLabelValues("error").Inc()
		return fmt.Errorf("timestamp %v for metric %q is too far in the future (more than 1 hour ahead of now)", ts, name)
	}

	last, err := s.lastTime(ctx, name)
	if err != nil {
		s.metricWrites.WithLabelValues("error").Inc()
		return fmt.Errorf("getting last time for metric %q: %w", name, err)
	}
	if !ts.After(last) {
		slog.Debug("skipping value with timestamp before last reported", "metric", name, "ts", ts, "last", last)
		s.metricWrites.WithLabelValues("skipped").Inc()
		return nil
	}

	req := &prompb.WriteRequest{
		Timeseries: []prompb.TimeSeries{
			{
				Labels: s.labelsProto(name),
				Samples: []prompb.Sample{
					{
						Value:     value,
						Timestamp: ts.UnixNano() / int64(time.Millisecond),
					},
				},
			},
		},
	}
	if s.config.DryRun {
		slog.Info("dry run, skipping write", "request", req)
		s.metricWrites.WithLabelValues("skipped").Inc()
	} else {
		if _, err := s.write.WriteProto(ctx, req); err != nil {
			s.metricWrites.WithLabelValues("error").Inc()
			return fmt.Errorf("sending request %+v: %w", req, err)
		}
	}
	s.metricWrites.WithLabelValues("success").Inc()
	s.lastTimes[name] = ts
	return nil
}

// labelSet returns the full label set for a metric.
func (s *Syncer) labelSet(metricName string) labels.Labels {
	ll := labels.Labels{
		{Name: "__name__", Value: s.config.MetricPrefix + metricName},
	}
	// Add additional labels, and sort by name.
	for name, value := range s.config.Labels {
		ll = append(ll, labels.Label{Name: name, Value: value})
	}
	slices.SortFunc(ll, func(a, b labels.Label) int {
		return strings.Compare(a.Name, b.Name)
	})
	return ll
}

// labelsProto returns the full label set for a metric as a protobuf.
func (s *Syncer) labelsProto(metricName string) []prompb.Label {
	return prompb.FromLabels(s.labelSet(metricName), nil)
}
