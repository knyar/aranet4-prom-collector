package main

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
)

type promSyncer struct {
	write  *promwrite.Client
	api    api.Client
	dryRun bool

	// lastTimes is a map of metric name to the last time it was written.
	lastTimes map[string]time.Time
}

func newPromSyncer(endpoint string, dryRun bool) (*promSyncer, error) {
	url, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to URL %q: %w", endpoint, err)
	}
	if url.Host == "" {
		return nil, fmt.Errorf("URL %q has no host", endpoint)
	}
	if url.Scheme == "" {
		return nil, fmt.Errorf("URL %q has no scheme", endpoint)
	}
	client, err := api.NewClient(api.Config{
		Address: url.String(),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create Prometheus client: %w", err)
	}

	writeURL := url.JoinPath("/api/v1/write")
	slog.Debug("Prometheus syncer created", "write-url", writeURL.String())
	return &promSyncer{
		write:     promwrite.NewClient(writeURL.String()),
		api:       client,
		dryRun:    dryRun,
		lastTimes: make(map[string]time.Time),
	}, nil
}

func (s *promSyncer) lastTime(ctx context.Context, metric string) (time.Time, error) {
	last, ok := s.lastTimes[metric]
	if ok {
		return last, nil
	}
	api := v1.NewAPI(s.api)
	query := fmt.Sprintf("timestamp(%s)", labelSet(metric).String())
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

func (s *promSyncer) reportMetric(ctx context.Context, name string, ts time.Time, value float64) error {
	if ts.IsZero() {
		return fmt.Errorf("cannot report metric %q with zero timestamp", name)
	}
	now := time.Now()
	if ts.After(now.Add(time.Hour)) {
		return fmt.Errorf("timestamp %v for metric %q is too far in the future (more than 1 hour ahead of now)", ts, name)
	}
	last, err := s.lastTime(ctx, name)
	if err != nil {
		return fmt.Errorf("getting last time for metric %q: %w", name, err)
	}
	if !ts.After(last) {
		slog.Debug("skipping value with timestamp before last reported", "metric", name, "ts", ts, "last", last)
		return nil
	}

	req := &prompb.WriteRequest{
		Timeseries: []prompb.TimeSeries{
			{
				Labels: labelsProto(name),
				Samples: []prompb.Sample{
					{
						Value:     value,
						Timestamp: ts.UnixNano() / int64(time.Millisecond),
					},
				},
			},
		},
	}
	if s.dryRun {
		slog.Info("dry run, skipping write", "request", req)
	} else {
		if _, err := s.write.WriteProto(ctx, req); err != nil {
			return fmt.Errorf("sending request %+v: %w", req, err)
		}
	}
	s.lastTimes[name] = ts
	return nil
}

func labelSet(metricName string) labels.Labels {
	ll := labels.Labels{
		{Name: "__name__", Value: *metricPrefix + metricName},
		{Name: "job", Value: *jobName},
		{Name: "instance", Value: *instanceName},
		{Name: "device_addr", Value: *deviceAddr},
	}
	return ll
}

func labelsProto(metricName string) []prompb.Label {
	l := prompb.FromLabels(labelSet(metricName), nil)
	slices.SortFunc(l, func(a, b prompb.Label) int {
		return strings.Compare(a.Name, b.Name)
	})
	return l
}
