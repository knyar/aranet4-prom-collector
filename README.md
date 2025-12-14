# aranet4-prom-collector

Collects air quality metrics from Aranet4 devices via Bluetooth and exports them to Prometheus using the Remote Write API.

The collector reads historical data from the device, avoids duplicate writes by tracking last reported timestamps, and provides a web interface for status monitoring and Bluetooth pairing passkey entry.
