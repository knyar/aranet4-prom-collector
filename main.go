package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"time"

	"github.com/knyar/aranet4-ble"
	"github.com/rigado/ble"
	"github.com/rigado/ble/linux"
	bonds "github.com/rigado/ble/linux/hci/bond"
)

var (
	hostname, _ = os.Hostname()

	verbose = flag.Bool("verbose", false, "Verbose logging")
	dryRun  = flag.Bool("dry-run", false, "Dry run mode")

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

	prom, err := newPromSyncer(*promEndpoint, *dryRun)
	if err != nil {
		slog.Error("failed to create Prometheus syncer", "error", err)
		os.Exit(1)
	}

	slog.Info("starting Aranet4 Prometheus collector", "device-addr", *deviceAddr)

	if err := refresh(prom); err != nil {
		slog.Error("failed to refresh", "error", err)
		os.Exit(1)
	}

	lastSuccess := time.Now()
	for {
		waitFor := time.Until(lastSuccess.Add(*interval))
		if waitFor > 0 {
			slog.Info("waiting for next interval", "wait_for", waitFor)
			time.Sleep(waitFor)
		} else {
			// Keep retrying if we're behind schedule.
			time.Sleep(time.Second)
		}
		if err := refresh(prom); err != nil {
			slog.Error("failed to refresh", "error", err)
		} else {
			lastSuccess = time.Now()
		}
	}
}

func refresh(prom *promSyncer) (retErr error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic in refresh", "error", r)
			retErr = fmt.Errorf("panic in refresh: %v", r)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	latest, all, err := readData(ctx)
	if err != nil {
		return fmt.Errorf("reading data: %w", err)
	}
	slog.Info("read data", "num_records", len(all))

	if latest.Battery > -1 {
		if err := prom.reportMetric(ctx, "battery_level_percent", latest.Time, float64(latest.Battery)); err != nil {
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
		if err := reportData(ctx, prom, &data); err != nil {
			return fmt.Errorf("reporting data: %w", err)
		}
		lastReported = data.Time
	}
	if !lastReported.IsZero() {
		if err := prom.reportMetric(ctx, "last_reported_time_seconds", time.Now(), float64(lastReported.Unix())); err != nil {
			return fmt.Errorf("reporting last success time: %w", err)
		}
	}
	return nil
}

func passkey() int {
	// prompt for passkey
	fmt.Print("Enter passkey: ")
	var p int
	n, err := fmt.Scanf("%d", &p)
	if err != nil || n != 1 {
		fmt.Printf("ERROR: expected 1 integer; got %d values (%v)\n", n, err)
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
		if err := device.Client().Pair(ble.AuthData{PasskeyFn: passkey}, 2*time.Minute); err != nil {
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

func reportData(ctx context.Context, prom *promSyncer, data *aranet4.Data) error {
	report := func(name string, value float64) error {
		return prom.reportMetric(ctx, name, data.Time, value)
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
