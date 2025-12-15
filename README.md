# aranet4-prom-collector

Collects air quality metrics from Aranet4 devices via Bluetooth and exports them to Prometheus using the Remote Write API.

Aranet4 [keeps on-device history](https://forum.aranet.com/aranet-home-devices-aranet4-aranet2-aranet-radiation-aranet-radon/how-long-does-the-aranet4-device-store-historic-data/) which `aranet4-prom-collector` syncs to Prometheus. It allows you to have a long-term archive of air quality measurements even if your monitor is only occasionally in bluetooth range of the collector.

## Usage

```bash
go build .
./aranet4-prom-collector -addr=<aranet4 bluetooth address> -prometheus-url=http://prometheus:9090
```

Please make sure your Prometheus server is configured to accept metrics through Remote Write API ([--web.enable-remote-write-receiver](https://prometheus.io/docs/prometheus/latest/querying/api/#remote-write-receiver)) and enable support for [out-of-order samples](https://prometheus.io/docs/prometheus/latest/configuration/configuration/#tsdb) (set `out_of_order_time_window` to `30d`).

During the first run the collector will attempt to pair with Aranet4 over Bluetooth.
For pairing, you will need to enter the 6-digit keypass either in terminal (if TTY is available), or on a web page (port 9090 by default).
Pairing details will be saved to the `bonds.json` file in current directory (use `-bt-bonds-file=` to
override).

## Running in gokrazy

To run the collector on a Raspberry Pi and sync metrics to a Prometheus server
over Tailscale once a day, use the following [gokrazy](https://gokrazy.org/) config:

```
{
    "Hostname": "aranet4-collector",
    "Packages": [
        "tailscale.com/cmd/tailscaled",
        "tailscale.com/cmd/tailscale",
        "github.com/gokrazy/bluetooth",
        "github.com/gokrazy/mkfs",
        "github.com/knyar/aranet4-prom-collector"
    ],
    "PackageConfig": {
        "github.com/knyar/aranet4-prom-collector": {
            "CommandLineFlags": [
                "-interval=24h",
                "-bt-bonds-file=/perm/aranet4-bt.json",
                "-prometheus-url=http://prometheus:9090",
                "-addr=AA:00:11:22:33:44"
            ]
        },
        "tailscale.com/cmd/tailscale": {
            "CommandLineFlags": [
                "up"
            ]
        }
    }
}
```

## Known issues

### unknown field PasskeyFn

When compiling for Gokrazy, you might hit an error:

> /Users/ryzh/code/gopath/pkg/mod/github.com/knyar/aranet4-prom-collector@v0.0.0-20251214185135-b8693e239b5b/main.go:262:28: unknown field PasskeyFn in struct literal of type ble.AuthData

For now, a patched `ble` library is used, so you need to add a `replace` statement to `go.mod`:

```bash
cd builddir/github.com/knyar/aranet4-prom-collector/
go mod edit -replace github.com/rigado/ble=github.com/knyar/ble@getPasskey
```
