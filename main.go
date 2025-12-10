package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/castai/promwrite"
	"github.com/prometheus/prometheus/prompb"
	"github.com/rigado/ble"
	"github.com/rigado/ble/linux"
	bonds "github.com/rigado/ble/linux/hci/bond"
	"sbinet.org/x/aranet4"
)

var (
	hostname, _ = os.Hostname()

	verbose = flag.Bool("verbose", false, "Verbose logging")

	hciSkt     = flag.Int("device", -1, "hci index")
	btBondFile = flag.String("bt-bonds-file", "bonds.json", "Bluetooth bond state file")
	deviceAddr = flag.String("addr", "F5:6C:BE:D5:61:47", "MAC address of Aranet4")

	sinceTime     = flag.String("since", "", "Start time for historical data")
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
	var since time.Time
	if *sinceTime != "" {
		var err error
		since, err = time.Parse(time.RFC3339, *sinceTime)
		if err != nil {
			slog.Error("failed to parse since time", "error", err)
			os.Exit(1)
		}
	}

	slog.Info("starting Aranet4 Prometheus collector", "device-addr", *deviceAddr, "interval", *interval, "metric-prefix", *metricPrefix, "write-endpoint", *writeEndpoint, "job-name", *jobName, "instance-name", *instanceName)

	last, err := refresh(since)
	if err != nil {
		slog.Error("failed to refresh", "error", err)
		os.Exit(1)
	}

	for {
		waitFor := time.Until(last.Add(*interval))
		if waitFor > 0 {
			slog.Info("waiting for next interval", "wait_for", waitFor)
			time.Sleep(waitFor)
		} else {
			// Keep retrying if we're behind schedule.
			time.Sleep(time.Second)
		}
		if l, err := refresh(last); err != nil {
			slog.Error("failed to refresh", "error", err)
		} else {
			last = l
		}
	}
}

func refresh(prev time.Time) (retTime time.Time, retErr error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic in refresh", "error", r)
			retTime = prev
			retErr = fmt.Errorf("panic in refresh: %v", r)
		}
	}()
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
	lastTime := prev
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
		lastTime = data.Time
	}
	if !lastTime.IsZero() {
		if err := reportMetric(ctx, client, "last_success_time_seconds", lastTime, float64(lastTime.Unix())); err != nil {
			return prev, fmt.Errorf("reporting last success time: %w", err)
		}
	}
	return lastTime, nil
}

func passkey() int {
	// prompt for passkey
	fmt.Print("Enter passkey: ")
	var p int
	n, err := fmt.Scanf("%d", &p)
	if err != nil || n != 1 {
		log.Printf("ERROR: expected 1 integer; got %d values (%v)", n, err)
		return passkey()
	}
	return p
}

func readData(ctx context.Context) (latest *aranet4.Data, all []aranet4.Data, _ error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	bm := bonds.NewBondManager(*btBondFile)

	d, err := linux.NewDevice(
		ble.OptEnableSecurity(bm),
		ble.OptTransportHCISocket(*hciSkt),
		ble.OptDialerTimeout(10*time.Second),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("can't init device: %w", err)
	}
	ble.SetDefaultDevice(d)
	defer d.Stop()

	slog.Debug("connecting to device", "device-addr", *deviceAddr)
	device, err := aranet4.New(ctx, *deviceAddr, aranet4.WithPasskey(passkey))
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
	// Validate timestamp - must not be zero
	if ts.IsZero() {
		return fmt.Errorf("cannot report metric %q with zero timestamp", name)
	}
	// Reject timestamps more than 1 hour in the future (likely clock sync issue)
	// Allow old timestamps for historical data
	now := time.Now()
	if ts.After(now.Add(time.Hour)) {
		return fmt.Errorf("timestamp %v for metric %q is too far in the future (more than 1 hour ahead of now)", ts, name)
	}

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
