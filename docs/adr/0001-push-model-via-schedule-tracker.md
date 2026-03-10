# ADR-0001: Push model via lucos_schedule_tracker

**Date:** 2026-03-10
**Status:** Accepted
**Discussed in:** [lucas42/lucos#45](https://github.com/lucas42/lucos/issues/45)

## Context

lucos runs Docker containers across multiple hosts (avalon, xwing, salvare). Some containers define Docker healthchecks, but there is no centralised visibility into which containers are healthy or unhealthy on each host. The existing monitoring system (`lucos_monitoring`) polls `/_info` endpoints over HTTPS to track service health, but this requires each service to be reachable via a domain name with TLS termination.

A new service, `lucos_docker_health`, is needed to monitor Docker container healthcheck status on each host and surface it through the existing monitoring infrastructure.

### Options considered

**Option A: Per-host `/_info` endpoint with dedicated domain.** Each host runs an HTTP server exposing `/_info`, registered in configy with its own domain (e.g. `docker-health-avalon.l42.eu`). Monitoring discovers and polls it like any other system.

- Works within the existing monitoring model
- Requires one DNS entry, one configy entry, and one router proxy rule per host
- Salvare does not currently run `lucos_router`, so this would require deploying router there or implementing independent TLS termination
- Does not scale cleanly -- each new host needs DNS, configy, and router configuration

**Option B: Per-host subpath via router.** Each host's router proxies `/docker-health/_info` to the local container. Monitoring polls the host domain with the subpath.

- No new domains needed
- Requires monitoring changes to support subpath polling (currently it only appends `/_info` to the domain)
- Still requires router on every host

**Option C: Push to `lucos_schedule_tracker`.** The service runs periodically on each host, reads the Docker socket, and POSTs its health status to schedule_tracker's `/report-status` endpoint. Each host uses a distinct `system` value (e.g. `lucos_docker_health_avalon`).

- No new domains, DNS entries, configy system entries, or router configuration needed
- Works immediately on all hosts, including salvare which has no router
- Reuses schedule_tracker's existing stale-check and error-counting logic
- Same Docker image deployed to every host, differentiated only by the `SYSTEM` environment variable

## Decision

**Option C: push model via `lucos_schedule_tracker`.**

lucas42 proposed this approach after the architect's initial recommendation of Option A was rejected for not scaling beyond a single host. The push model eliminates all per-host infrastructure requirements beyond the container itself.

### Design

A single Go binary runs as a Docker container on each host. Every 60 seconds, it:

1. Reads the Docker socket (mounted read-only) to list all running containers and their health status
2. If any container with a healthcheck is `unhealthy`: POSTs to schedule_tracker with `status: "error"` and a `message` listing the unhealthy container names
3. If all containers with healthchecks are `healthy`: POSTs with `status: "success"`
4. Containers without a healthcheck are excluded from the checks entirely -- their count is not reported (this is a runtime health service, not a deployment audit)

The `system` field sent to schedule_tracker includes the hostname: `lucos_docker_health_avalon`, `lucos_docker_health_xwing`, `lucos_docker_health_salvare`. This gives monitoring per-host granularity.

The reporting frequency of 60 seconds gives schedule_tracker a 3-minute stale threshold (its default `frequency * 3` rule). If the docker_health container stops running, the check goes stale and monitoring flags it -- better failure-mode coverage than the polling model, where a dead service simply produces a fetch error.

### Deployment

Same Docker image on every host. Per-host configuration is limited to:

- `SYSTEM` environment variable (e.g. `lucos_docker_health_avalon`)
- `SCHEDULE_TRACKER_URL` environment variable (the `/report-status` endpoint, from lucos_creds)

The container mounts the Docker socket read-only (`/var/run/docker.sock:/var/run/docker.sock:ro`) and runs with no other volumes. No state is persisted.

### Security

- Docker socket mounted `:ro` (prevents replacing or deleting the socket file; does not restrict API calls, but limits filesystem-level tampering)
- Application code calls only read-only Docker API endpoints (`/containers/json`)
- Container runs as a non-root user
- Minimal/distroless base image -- no Docker CLI, no shell
- No `--privileged` flag, no extra capabilities

### Container and image naming

- Container name: `lucos_docker_health` (one per host, no ambiguity)
- Image name: `lucas42/lucos_docker_health`

### Language choice

Go. Produces a single static binary suitable for a scratch/distroless image (a few MB). The Docker SDK for Go (`docker/docker`) is the reference implementation. Go is already used in `lucos_repos`, `lucos_creds`, `lucos_configy`, and `lucos_media_metadata_api`.

## Consequences

### Positive

- Works on all hosts immediately, including salvare (no router, no TLS, no DNS needed)
- Same image everywhere -- deploy is trivial and consistent
- Reuses schedule_tracker's existing monitoring integration (stale detection, error counting)
- If docker_health itself fails, the stale-check mechanism catches it automatically
- Small, stateless container with minimal attack surface

### Negative

- `lucos_schedule_tracker` becomes a dependency for all Docker health monitoring. If schedule_tracker is down, no host can report. Monitoring will eventually flag the checks as stale, which is the correct behaviour (visibility has been lost), but there is a gap between schedule_tracker going down and the stale threshold being reached.
- Per-host granularity only -- individual container health is reported in the error `message` text, not as separate check entries. Per-container monitoring remains the responsibility of each service's own `/_info` endpoint.
- Unlike the `/_info` polling model, there is no way to query a host's Docker health on demand. The data is only as fresh as the last push (up to 60 seconds stale).
