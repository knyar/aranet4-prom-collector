package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"time"

	"github.com/knyar/aranet4-ble"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rigado/ble"
	"github.com/rigado/ble/linux"
	bonds "github.com/rigado/ble/linux/hci/bond"
	"tailscale.com/syncs"

	"github.com/knyar/aranet4-prom-collector/promsync"
)

var (
	hostname, _ = os.Hostname()

	verbose = flag.Bool("verbose", false, "Verbose logging")
	dryRun  = flag.Bool("dry-run", false, "Dry run mode")
	listen  = flag.String("listen", ":9090", "Listen address for HTTP server")

	hciSocketID = flag.Int("hci-socket-id", -1, "hci device socket ID")
	deviceAddr  = flag.String("addr", "F5:6C:BE:D5:61:47", "MAC address of Aranet4")
	btBondFile  = flag.String("bt-bonds-file", "bonds.json", "Bluetooth bond state file: written when pairing is successful")

	interval     = flag.Duration("interval", time.Hour, "How often to sync data from Aranet4 to Prometheus")
	metricPrefix = flag.String("prefix", "aranet4_", "Prefix for metrics")
	promEndpoint = flag.String("prometheus-url", "http://localhost:9090/", "Prometheus base URL")
	jobName      = flag.String("job", "aranet4", "Job name for metrics")
	instanceName = flag.String("instance", hostname, "Instance name for metrics")
)

func main() {
	flag.Parse()

	if *verbose {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}

	prom, err := promsync.New(promsync.Config{
		PrometheusEndpoint: *promEndpoint,
		MetricPrefix:       *metricPrefix,
		Labels: map[string]string{
			"job":         *jobName,
			"instance":    *instanceName,
			"device_addr": *deviceAddr,
		},
		DryRun: *dryRun,
	})
	if err != nil {
		slog.Error("failed to create Prometheus syncer", "error", err)
		os.Exit(1)
	}

	slog.Info("starting Aranet4 Prometheus collector", "device-addr", *deviceAddr, "listen", *listen)
	c, err := newCollector(prom)
	if err != nil {
		slog.Error("failed to create collector", "error", err)
		os.Exit(1)
	}

	c.run()
}

type collector struct {
	prom *promsync.Syncer

	// lastSuccess is the last time the collector successfully refreshed data.
	lastSuccess syncs.AtomicValue[time.Time]

	// lastReported is the timestamp of the last reported measurement.
	lastReported syncs.AtomicValue[time.Time]

	attempts *prometheus.HistogramVec
}

func newCollector(prom *promsync.Syncer) (*collector, error) {
	c := &collector{
		prom: prom,
		attempts: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    *metricPrefix + "refresh_latencies_seconds",
			Help:    "Latencies of refresh attempts.",
			Buckets: prometheus.ExponentialBucketsRange(1, 120, 5),
		}, []string{"status"}),
	}
	if err := c.refresh(); err != nil {
		return nil, fmt.Errorf("failed to refresh: %w", err)
	}
	promauto.NewGaugeFunc(prometheus.GaugeOpts{
		Name: *metricPrefix + "last_success_time_seconds",
		Help: "The last time the collector successfully refreshed data.",
	}, func() float64 {
		return float64(c.lastSuccess.Load().Unix())
	})

	http.Handle("/", c)
	http.Handle("/metrics", promhttp.Handler())
	go func() {
		http.ListenAndServe(*listen, nil)
	}()
	return c, nil
}

func (c *collector) run() {
	for {
		waitFor := time.Until(c.lastSuccess.Load().Add(*interval))
		if waitFor > 0 {
			slog.Info("waiting for next interval", "wait_for", waitFor)
			time.Sleep(waitFor)
		} else {
			// Keep retrying if we're behind schedule.
			time.Sleep(time.Second)
		}
		if err := c.refresh(); err != nil {
			slog.Error("failed to refresh", "error", err)
		}
	}
}

func (c *collector) refresh() (retErr error) {
	t0 := time.Now()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic in refresh", "error", r)
			retErr = fmt.Errorf("panic in refresh: %v", r)
		}
		status := "success"
		if retErr != nil {
			status = "error"
		}
		c.attempts.WithLabelValues(status).Observe(time.Since(t0).Seconds())
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	latest, all, err := c.readData(ctx)
	if err != nil {
		return fmt.Errorf("reading data: %w", err)
	}
	slog.Info("read data", "num_records", len(all))

	if latest.Battery > -1 {
		if err := c.prom.ReportMetric(ctx, "battery_level_percent", latest.Time, float64(latest.Battery)); err != nil {
			return fmt.Errorf("reporting battery level: %w", err)
		}
	}

	if len(all) == 0 {
		slog.Warn("no history read", "num_records", len(all))
		return nil
	}
	slices.SortFunc(all, func(a, b aranet4.Data) int {
		return a.Time.Compare(b.Time)
	})
	var lastReported time.Time
	for _, data := range all {
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
		if err := c.reportData(ctx, &data); err != nil {
			return fmt.Errorf("reporting data: %w", err)
		}
		lastReported = data.Time
	}
	if !lastReported.IsZero() {
		c.lastReported.Store(lastReported)
	}
	c.lastSuccess.Store(time.Now())
	return nil
}

func (c *collector) readData(ctx context.Context) (latest *aranet4.Data, all []aranet4.Data, _ error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	bm := bonds.NewBondManager(*btBondFile)

	d, err := linux.NewDevice(
		ble.OptEnableSecurity(bm),
		ble.OptTransportHCISocket(*hciSocketID),
		ble.OptDialerTimeout(10*time.Second),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("can't init device: %w", err)
	}
	ble.SetDefaultDevice(d)
	defer d.Stop()

	slog.Debug("connecting to device", "device-addr", *deviceAddr)
	device, err := aranet4.New(ctx, *deviceAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to device: %w", err)
	}
	defer device.Close()

	// Start encryption.
	addr := device.Client().Addr().Bytes()
	// Reverse the address bytes (Bluetooth addresses are stored in little-endian,
	// but the bond manager expects big-endian format)
	slices.Reverse(addr)
	for !bm.Exists(hex.EncodeToString(addr)) {
		slog.Warn("no bond found, pairing")
		authData := ble.AuthData{PasskeyFn: func() int { return c.passkey(ctx) }}
		if err := device.Client().Pair(authData, 2*time.Minute); err != nil {
			return nil, nil, fmt.Errorf("pairing: %w", err)
		}
	}

	slog.Debug("starting encryption")
	m := make(chan ble.EncryptionChangedInfo)
	if err := device.Client().StartEncryption(m); err != nil {
		return nil, nil, fmt.Errorf("starting encryption: %w", err)
	}

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

func (c *collector) reportData(ctx context.Context, data *aranet4.Data) error {
	report := func(name string, value float64) error {
		return c.prom.ReportMetric(ctx, name, data.Time, value)
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

func (c *collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {

}

func (c *collector) passkey(ctx context.Context) int {
	// prompt for passkey
	fmt.Print("Enter passkey: ")
	var p int
	n, err := fmt.Scanf("%d", &p)
	if err != nil || n != 1 {
		fmt.Printf("ERROR: expected 1 integer; got %d values (%v)\n", n, err)
		return c.passkey(ctx)
	}
	return p
}
