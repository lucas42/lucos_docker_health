# CLAUDE.md — lucos_docker_health

## Overview

A Go binary that runs on each Docker host, reads healthcheck status from the Docker socket, and reports to `lucos_schedule_tracker` every 60 seconds (push model). There is no HTTP server — this service only ever makes outbound HTTP requests.

See [ADR-0001](docs/adr/0001-push-model-via-schedule-tracker.md) for the architectural decision.

## Language

Go. Single binary, no CGO (`CGO_ENABLED=0`), suitable for a scratch/distroless image.

## Architecture

Push model: the binary polls Docker, then POSTs to `SCHEDULE_TRACKER_ENDPOINT` with a JSON payload containing `system`, `frequency`, and `status` (`"success"` or `"error"`). No inbound ports. No state persisted.

Containers without a configured healthcheck are ignored entirely.

## Docker socket

The Docker socket is mounted read-only (`/var/run/docker.sock:/var/run/docker.sock:ro`). The binary only calls `ContainerList` and `ContainerInspect` — no write operations.

## Environment variables

| Variable | Required | Description |
|---|---|---|
| `SYSTEM` | Yes | Base system name (e.g. `lucos_docker_health`) — combined with `HOSTDOMAIN` prefix to form the per-host identifier |
| `HOSTDOMAIN` | Yes | Host domain (e.g. `avalon.s.l42.eu`) — prefix before first `.` is appended to `SYSTEM` (e.g. `lucos_docker_health_avalon`) |
| `SCHEDULE_TRACKER_ENDPOINT` | Yes | Full URL to the `/report-status` endpoint |
| `REPORT_FREQUENCY` | No | Reporting interval in seconds (default: 60) |

`SYSTEM`, `HOSTDOMAIN`, and `SCHEDULE_TRACKER_ENDPOINT` are provided by lucos_creds with per-host values.

## Tests

There are no automated tests at this time. The binary is small and straightforward; coverage is provided by CI build verification and production monitoring (stale-check via schedule_tracker).
