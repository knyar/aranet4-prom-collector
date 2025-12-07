package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/castai/promwrite"
	"github.com/prometheus/prometheus/prompb"
	"sbinet.org/x/aranet4"
)

var (
	hostname, _ = os.Hostname()

	verbose       = flag.Bool("verbose", false, "Verbose logging")
	deviceAddr    = flag.String("addr", "F5:6C:BE:D5:61:47", "MAC address of Aranet4")
	interval      = flag.Duration("interval", time.Hour, "How often to read data from Aranet4")
	metricPrefix  = flag.String("prefix", "aranet4_", "Prefix for metrics")
	writeEndpoint = flag.String("write-endpoint", "http://localhost:9090/api/v1/write", "Prometheus Remote Write API endpoint for metrics")
	jobName       = flag.String("job", "aranet4", "Job name for metrics")
	instanceName  = flag.String("instance", hostname, "Instance name for metrics")
)

func main() {
	flag.Parse()

	if *verbose {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}

	slog.Info("starting Aranet4 Prometheus collector", "device-addr", *deviceAddr, "interval", *interval, "metric-prefix", *metricPrefix, "write-endpoint", *writeEndpoint, "job-name", *jobName, "instance-name", *instanceName)

	last, err := refresh(time.Time{})
	if err != nil {
		slog.Error("failed to refresh", "error", err)
		os.Exit(1)
	}

	for {
		waitFor := time.Until(last.Add(*interval))
		if waitFor > 0 {
			slog.Info("waiting for next interval", "wait_for", waitFor)
			time.Sleep(waitFor)
		}
		if l, err := refresh(last); err != nil {
			slog.Error("failed to refresh", "error", err)
		} else {
			last = l
		}
	}
}

func refresh(prev time.Time) (time.Time, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	client := promwrite.NewClient(*writeEndpoint)

	latest, all, err := readData(ctx)
	if err != nil {
		return prev, fmt.Errorf("reading data: %w", err)
	}
	slog.Info("read data", "num_records", len(all))

	if latest.Battery > -1 {
		if err := reportMetric(ctx, client, "battery_level_percent", latest.Time, float64(latest.Battery)); err != nil {
			return prev, fmt.Errorf("reporting battery level: %w", err)
		}
	}

	if len(all) == 0 {
		slog.Warn("no history read", "num_records", len(all))
		return prev, nil
	}
	slices.SortFunc(all, func(a, b aranet4.Data) int {
		return a.Time.Compare(b.Time)
	})
	for _, data := range all {
		if !prev.IsZero() && !data.Time.After(prev) {
			continue
		}
		if data.Time.IsZero() {
			slog.Warn("unexpected time value, skipping", "data", data)
			continue
		}
		if data.CO2 <= 0 {
			slog.Warn("unexpected CO2 value, skipping", "data", data)
			continue
		}
		if data.P <= 0 {
			slog.Warn("unexpected pressure value, skipping", "data", data)
			continue
		}
		slog.Debug("reporting new record", "data", data)
		if err := reportData(ctx, client, &data); err != nil {
			return prev, fmt.Errorf("reporting data: %w", err)
		}
	}
	lastTime := all[len(all)-1].Time
	if err := reportMetric(ctx, client, "last_success_time_seconds", lastTime, float64(lastTime.Unix())); err != nil {
		return prev, fmt.Errorf("reporting last success time: %w", err)
	}
	return lastTime, nil
}

func readData(ctx context.Context) (latest *aranet4.Data, all []aranet4.Data, _ error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	slog.Debug("connecting to device", "device-addr", *deviceAddr)
	device, err := aranet4.New(ctx, *deviceAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to device: %w", err)
	}
	defer device.Close()

	slog.Debug("reading latest data")
	data, err := device.Read()
	if err != nil {
		return nil, nil, fmt.Errorf("reading latest data: %w", err)
	}

	slog.Debug("read data", "data", data)

	slog.Debug("reading historic data")
	allData, err := device.ReadAll()
	if err != nil {
		return nil, nil, fmt.Errorf("reading historic data: %w", err)
	}
	return &data, allData, nil
}

func reportData(ctx context.Context, client *promwrite.Client, data *aranet4.Data) error {
	report := func(name string, value float64) error {
		return reportMetric(ctx, client, name, data.Time, value)
	}
	if err := report("co2_ppm", float64(data.CO2)); err != nil {
		return fmt.Errorf("reporting CO2: %w", err)
	}
	if err := report("humidity_percent", data.H); err != nil {
		return fmt.Errorf("reporting humidity: %w", err)
	}
	if err := report("pressure_hpa", data.P); err != nil {
		return fmt.Errorf("reporting pressure: %w", err)
	}
	if err := report("temperature_celsius", data.T); err != nil {
		return fmt.Errorf("reporting temperature: %w", err)
	}
	return nil
}

func reportMetric(ctx context.Context, client *promwrite.Client, name string, ts time.Time, value float64) error {
	req := &prompb.WriteRequest{
		Timeseries: []prompb.TimeSeries{
			{
				Labels: labels(name),
				Samples: []prompb.Sample{
					{
						Value:     value,
						Timestamp: ts.UnixNano() / int64(time.Millisecond),
					},
				},
			},
		},
	}
	_, err := client.WriteProto(ctx, req)
	if err != nil {
		return fmt.Errorf("sending request %+v: %w", req, err)
	}
	return nil
}

func labels(metricName string) []prompb.Label {
	l := make([]prompb.Label, 0, 4)
	l = append(l, prompb.Label{
		Name:  "__name__",
		Value: *metricPrefix + metricName,
	})
	l = append(l, prompb.Label{
		Name:  "job",
		Value: *jobName,
	})
	l = append(l, prompb.Label{
		Name:  "instance",
		Value: *instanceName,
	})
	l = append(l, prompb.Label{
		Name:  "device_addr",
		Value: *deviceAddr,
	})
	slices.SortFunc(l, func(a, b prompb.Label) int {
		return strings.Compare(a.Name, b.Name)
	})
	return l
}
