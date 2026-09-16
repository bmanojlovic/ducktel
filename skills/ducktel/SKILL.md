---
name: ducktel
description: >
  Work with ducktel — a lightweight, single-binary OpenTelemetry backend for LLM agent diagnostics.
  Use when querying OTLP traces/logs/metrics via the ducktel CLI, writing DuckDB SQL against Parquet-stored
  telemetry, diagnosing issues from OTel data, developing the ducktel Go codebase, or running the OTLP receiver.
  TRIGGER when: code imports ducktel packages, user mentions ducktel CLI commands (serve, query, traces, logs,
  metrics, schema, services), or user works with the ducktel project directory.
---

# ducktel

A lightweight, single-binary OpenTelemetry backend. Receives OTLP/HTTP, stores to Parquet, queries via embedded DuckDB.

## Architecture

```
OTel-instrumented apps
    ↓ OTLP/HTTP (protobuf or JSON)
ducktel serve  (receiver → buffer → Parquet writer)
    ↓
data/{traces,logs,metrics}/YYYY-MM-DD/HH-MM.parquet
    ↓
ducktel query/traces/logs/metrics  (embedded DuckDB)
    ↓
JSON / table / CSV output → LLM agents or humans
```

## CLI Reference

All commands accept `--data-dir` (default: `./data`) and `--format json|table|csv` (default: `json`).

### `ducktel serve`

Start the OTLP HTTP receiver.

```bash
ducktel serve                          # default: localhost:4318, flush every 30s
ducktel serve --port 9090              # custom port
ducktel serve --host 0.0.0.0           # all interfaces (containers)
ducktel serve --flush-interval 10s     # flush every 10 seconds
ducktel serve --auth-token secret      # require a bearer token
```

Endpoints: `POST /v1/traces`, `POST /v1/logs`, `POST /v1/metrics`, `GET /health`.

#### Authentication

Disabled unless a token is configured. Set it with `--auth-token` or
`DUCKTEL_AUTH_TOKEN` (an explicit flag wins over the environment):

```bash
DUCKTEL_AUTH_TOKEN=secret ducktel serve --host 0.0.0.0
```

Senders must then present `Authorization: Bearer <token>`. Every OTel SDK
supports this via one environment variable, so no instrumentation changes are
needed:

```bash
export OTEL_EXPORTER_OTLP_HEADERS="Authorization=Bearer secret"
```

- OTLP carries no credentials in the message body — per the spec, auth is
  transport-level, hence headers.
- `GET /health` stays unauthenticated so liveness/readiness probes keep working.
- A 401 is returned for a missing, malformed, or wrong credential. The scheme
  name is case-insensitive; the `Bearer ` prefix is required.
- **TLS is not terminated by ducktel.** If the token would cross an untrusted
  network, front the receiver with a proxy or ingress. OTLP/HTTP is
  request/response rather than a persistent connection, so a terminating proxy is
  a natural fit.
- Starting without a token while bound to a non-loopback address logs a warning.

### `ducktel query [sql]`

Execute raw DuckDB SQL against the `traces`, `logs`, or `metrics` views.

```bash
ducktel query "SELECT * FROM traces LIMIT 5"
ducktel query "SELECT service_name, count(*) FROM traces GROUP BY 1" --format table
```

### `ducktel traces`

Query traces with convenience filters.

| Flag | Description | Example |
|------|-------------|---------|
| `--service` | Filter by service name | `--service api-gateway` |
| `--since` | Spans newer than duration | `--since 1h`, `--since 30m` |
| `--status` | Filter by status: error, ok, unset | `--status error` |
| `--limit` | Max spans (default: 20) | `--limit 100` |

### `ducktel logs`

Query logs with convenience filters.

| Flag | Description | Example |
|------|-------------|---------|
| `--service` | Filter by service name | `--service payment-svc` |
| `--since` | Logs newer than duration | `--since 2h` |
| `--severity` | Filter: debug, info, warn, error, fatal | `--severity error` |
| `--search` | Case-insensitive body text search | `--search "timeout"` |
| `--limit` | Max logs (default: 50) | `--limit 200` |

### `ducktel metrics`

Query metrics with convenience filters.

| Flag | Description | Example |
|------|-------------|---------|
| `--service` | Filter by service name | `--service web-frontend` |
| `--since` | Metrics newer than duration | `--since 4h` |
| `--name` | Filter by metric name | `--name http.request.duration` |
| `--type` | Filter: gauge, sum, histogram, summary | `--type histogram` |
| `--limit` | Max points (default: 50) | `--limit 100` |

### `ducktel schema [view]`

Show column names and types. Default view: `traces`. Valid: `traces`, `logs`, `metrics`.

### `ducktel services`

List distinct service names from traces.

### `ducktel mcp`

Serve telemetry queries over the Model Context Protocol on stdio, for agent consumers.

```bash
ducktel mcp --data-dir /data
```

Client configuration (spawned as a subprocess by the agent):

```json
{"command": "ducktel", "args": ["mcp", "--data-dir", "/data"]}
```

Three generic, domain-agnostic tools. They know about spans, attributes, metrics and
time, and attach no meaning to any attribute key — domain interpretation is the
caller's job.

| Tool | Purpose |
|------|---------|
| `trace_lookup(trace_id, tenant)` | every span of one trace, ordered by start time |
| `span_search(filters, service_name, since_minutes, tenant, limit)` | spans matching attribute key/value filters |
| `metric_query(metric_name, aggregation, since_minutes, group_by, tenant)` | metric aggregation: avg, sum, min, max, count, p50, p95, p99 |

**Tenant isolation is mandatory.** `tenant` is a *required* parameter on every tool —
it is in the JSON schema, so a client cannot omit it — and every query hard-filters on
the `tenant.id` resource attribute. A caller can restrict its scope but never widen it.

**Dotted attribute keys work.** `span_search` takes filters as key/value pairs and
quotes the key internally, because DuckDB treats `$.service.name` as field "service"
then "name" and silently returns NULL. See the note under SQL Query Patterns.

**Time windows default to 60 minutes.** Pass `since_minutes` for a wider range. There is
no retention or pruning in ducktel, so unbounded scans would otherwise read all history.

**Transport is stdio**, so the process boundary is the trust boundary and no token is
needed. If HTTP transport is added later it should reuse the receiver's bearer token.


## SQL Query Patterns

The query engine uses embedded DuckDB. Three views are available: `traces`, `logs`, `metrics`.

For schema details, see [references/schema.md](references/schema.md).
For common query patterns, see [references/queries.md](references/queries.md).

## Key Conventions

- **Timestamps** are Unix microseconds (`int64`). Use `epoch_us(ts)` in DuckDB to convert.
- **duration_ms** is precomputed as `float64` milliseconds.
- **Attributes** (attributes, resource_attributes, events, links, exemplars) are stored as JSON strings. Query them with DuckDB JSON functions — but **quote dotted keys**: `json_extract_string(attributes, '$."http.method"')`. DuckDB reads `$.http.method` as field `http` then field `method` and returns NULL, so the unquoted form silently matches nothing for every OTel semantic-convention key. `attributes ->> 'http.method'` also works. See [references/schema.md](references/schema.md).
- **Status codes** are strings: `STATUS_CODE_OK`, `STATUS_CODE_ERROR`, `STATUS_CODE_UNSET`.
- **Span kinds** are strings: `SPAN_KIND_SERVER`, `SPAN_KIND_CLIENT`, `SPAN_KIND_PRODUCER`, `SPAN_KIND_CONSUMER`, `SPAN_KIND_INTERNAL`.
- **Metric types** are lowercase strings: `gauge`, `sum`, `histogram`, `summary`.
- **`metric_query` only aggregates `value_double`, which gauge and sum populate.** Histogram points keep their data in `sum`/`min`/`max`/`count`/`bucket_counts` and are invisible to `metric_query` — it returns `0`, not an error. So send a per-event duration or size as a **gauge** (or a counter as a **sum**), not a histogram, if you want it aggregatable. Histograms are still stored and can be read with raw SQL over the `metrics` table.
- **Filter values are bound, not interpolated.** The `traces`/`logs`/`metrics` commands pass filter values to DuckDB as parameters, so a service name or search term containing quotes is matched literally — it cannot alter the query. `--limit` is validated and capped at 10000.
- **`query` and `saved run` execute raw SQL by design.** There is no sandbox on `ducktel query "<sql>"`; it runs whatever you give it, including DDL. That is intentional (it is the SQL escape hatch for agents), so treat its input as trusted.

## Development

For project structure and development guide, see [references/development.md](references/development.md).
