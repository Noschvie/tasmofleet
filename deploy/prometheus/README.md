# Prometheus scrape example

`/metrics` needs a bearer token unless the scrape comes from a direct loopback
peer (no forwarding headers). Set the token on tasmofleet with
`TASMOFLEET_METRICS_TOKEN` or, preferably, `TASMOFLEET_METRICS_TOKEN_FILE`
(e.g. a 0600 file under `/data`). Give Prometheus the same value in a file.
Never inline it in `prometheus.yml`.

```yaml
scrape_configs:
  - job_name: tasmofleet
    scheme: http                  # or https when tasmofleet serves TLS itself
    metrics_path: /metrics
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/secrets/tasmofleet-metrics-token
    static_configs:
      - targets: ["172.31.12.61:8080"]   # example: container IP, UI port

  # Liveness via the blackbox exporter: /healthz needs no token.
  - job_name: tasmofleet-healthz
    metrics_path: /probe
    params:
      module: [http_2xx]
    static_configs:
      - targets: ["http://172.31.12.61:8080/healthz"]
    relabel_configs:
      - source_labels: [__address__]
        target_label: __param_target
      - target_label: __address__
        replacement: blackbox-exporter:9115
```

Per-device labels on the backup gauge are off by default
(the setting "Device names in metric labels" on the System settings page, off by default, reports one `device="fleet"` series:
the oldest "latest backup" of the fleet).
