# syntax=docker/dockerfile:1

# The build variant is ONE variable. KV_VARIANT selects the Go build tag here
# AND the image tag in docker-compose.yaml, so it is not possible to build the
# patched binary and label it :buggy -- which would produce a false green that
# looks exactly like a true green, the worst possible failure for a tool whose
# whole claim is "we found the known bug".
#
# As a second, independent check the binary reports its own compiled-in variant
# at GET /status, derived from the build tag rather than from any environment
# value, so a mislabelled image still tells the truth about itself.

FROM golang:1.22-alpine AS build
WORKDIR /src
# The module has no require block and no go.sum, so there is nothing to
# download and nothing to cache separately from the source.
COPY . .

ARG KV_VARIANT=buggy
RUN set -eu; \
    case "$KV_VARIANT" in \
      buggy)   TAGS="" ;; \
      kvfixed) TAGS="kvfixed" ;; \
      *) echo "KV_VARIANT must be 'buggy' or 'kvfixed', got '$KV_VARIANT'" >&2; exit 1 ;; \
    esac; \
    CGO_ENABLED=0 go build -tags "$TAGS" -trimpath -ldflags="-s -w" -o /out/kv ./cmd/kv; \
    CGO_ENABLED=0 go build -tags "$TAGS" -trimpath -ldflags="-s -w" -o /out/provebug ./cmd/provebug; \
    CGO_ENABLED=0 go build              -trimpath -ldflags="-s -w" -o /out/loadgen  ./cmd/loadgen; \
    /out/kv version

# Runtime image: one static binary, no shell utilities, no curl. The health
# probe is a subcommand of the binary itself.
#
# Deliberately NO NET_ADMIN and no iptables/iproute2: PRO-THESIS injects network
# faults from a sidecar joined to this container's network namespace at run
# time, so the target stays unmodified.
FROM alpine:3.20 AS kv
COPY --from=build /out/kv /kv
ENV KV_TICK_MS=25 \
    KV_HEARTBEAT_MS=100 \
    KV_ELECTION_MIN_MS=600 \
    KV_ELECTION_MAX_MS=900 \
    KV_RPC_TIMEOUT_MS=250 \
    KV_APPLY_WAIT_MS=2000 \
    KV_CLIENT_ADDR=0.0.0.0:8080 \
    KV_PEER_ADDR=0.0.0.0:9090 \
    KV_DATA_DIR=/var/lib/kv \
    KV_ROLE_HINT=replica
# NOTE: there is deliberately no KV_LEASE_MS. The read-lease duration is a
# compile-time constant in internal/raft/lease.go so that the injected defect
# cannot be turned off, or quietly made worse, by configuration.
EXPOSE 8080 9090
ENTRYPOINT ["/kv", "serve"]

# Optional image carrying the host-side tools, for environments where building
# them on the host is inconvenient.
FROM alpine:3.20 AS tools
COPY --from=build /out/loadgen /loadgen
COPY --from=build /out/provebug /provebug
ENTRYPOINT ["/loadgen"]
