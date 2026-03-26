# Observability and Telemetry

**Priority:** Low
**Effort:** Medium-High
**Inspired by:** Squad's OpenTelemetry integration and Aspire dashboard

## Problem

For larger deployments running many tasks, understanding system behavior requires
parsing log output, reading `.mato/messages/` files, and manually correlating
events. There's no structured telemetry for tracking task durations, success rates,
model costs, or system health over time.

## Idea

Add optional structured telemetry that emits spans and metrics for the task
lifecycle, enabling integration with standard observability tools.

## What Would Be Tracked

### Spans (Tracing)

- `mato.poll_cycle` -- one span per polling loop iteration
- `mato.task.claim` -- task selection and claiming
- `mato.task.run` -- full agent run (Docker launch to exit)
- `mato.task.review` -- review agent lifecycle
- `mato.task.merge` -- squash-merge processing
- `mato.reconcile` -- dependency reconciliation pass
- `mato.cleanup` -- orphan recovery, stale lock cleanup

### Metrics (Counters/Histograms)

- `mato.tasks.completed` (counter, by task ID)
- `mato.tasks.failed` (counter, by failure reason)
- `mato.tasks.duration_seconds` (histogram)
- `mato.reviews.approved` / `mato.reviews.rejected` (counters)
- `mato.merge.duration_seconds` (histogram)
- `mato.merge.conflicts` (counter)
- `mato.queue.depth` (gauge, by state)
- `mato.agents.active` (gauge)

### Configuration

```yaml
# .mato.yaml
telemetry:
  enabled: true
  endpoint: "http://localhost:4317"  # OTLP gRPC endpoint
  service_name: "mato"
  sample_rate: 1.0
```

Or via environment variable:
```bash
MATO_TELEMETRY_ENDPOINT=http://localhost:4317
```

## Design Considerations

- This should be fully optional with zero overhead when disabled.
- Use OpenTelemetry Go SDK (`go.opentelemetry.io/otel`) for standards compliance.
- Start with OTLP export only; specific backend support (Jaeger, Prometheus)
  comes through the OTLP collector.
- Keep the dependency footprint minimal -- the OTLP exporter and SDK are the
  only required additions.
- Consider a simpler alternative first: structured JSON log lines to stdout
  that can be parsed by existing log aggregation tools. This avoids the OTEL
  dependency while still providing machine-readable telemetry.
- `mato status --format json` already provides point-in-time snapshots. Telemetry
  adds the time-series dimension.

## Relationship to Existing Features

- Extends `mato status` (point-in-time) with continuous monitoring.
- Extends `mato doctor` (health checks) with runtime health metrics.
- The structured JSON log alternative could use the same event format as
  `messaging.WriteMessage` for consistency.
