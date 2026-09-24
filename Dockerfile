# PRO-THESIS as a container, so a project can be gated without building the
# harness from source.
#
# The harness drives the target through the docker CLI (internal/harness sets
# dockerBin = "docker"), so this image carries the client and talks to the
# HOST's daemon through a mounted socket. It does not run a daemon of its own.
# That has three consequences, all of which are contract, not detail:
#
#  1. A compose BUILD CONTEXT is streamed by the client out of THIS container's
#     filesystem, so it works from any mount path. Measured: `docker compose
#     build` on the reference fixture, run from inside this image with the
#     socket mounted, exits 0 and produces the image. A BIND-MOUNT volume is the
#     opposite case: the daemon resolves it against the HOST filesystem, so
#     `- ./data:/data` in a target's compose file needs that path to exist on
#     the host as written. Named volumes are unaffected, and the fixture uses
#     named volumes.
#  2. The target's published ports are on the host's network namespace, not
#     this container's, so PROTHESIS_PROBE_HOST must name an address this
#     container can reach them on (D-087). Loopback is wrong here and is the
#     default only because it is right on the host.
#  3. The driver and the oracles are the TARGET's executables. They are not in
#     this image and cannot be: they are built from the target's repository.
#     They have to be present under the mounted project, and runnable on
#     linux/amd64.
#
# See docs/TARGETS.md for the run recipe and what each of those means in
# practice.

FROM golang:1.22-bookworm AS build
WORKDIR /src

# Module files first, so a source-only change does not refetch the module graph.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# -trimpath for the same reason scripts/build.ps1 uses it: a path baked into the
# binary makes two builds of identical source differ, and the fixture's checker
# is fingerprinted by SHA-256 in .prothesis/lock (D-072). This image does not
# stamp commit provenance; `thesis version` reports an unstamped build and says
# so, which is honest rather than a made-up stamp.
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false -o /out/thesis ./cmd/thesis \
 && CGO_ENABLED=0 go build -trimpath -buildvcs=false -o /out/thesis-oracle-linearizable ./cmd/thesis-oracle-linearizable

# The official docker CLI image already carries the client and the compose v2
# plugin, which is what the harness shells out to. Building on it beats adding
# Docker's apt repository by hand: fewer moving parts, and the CLI version is
# pinned by a tag rather than by whatever the repository serves that day.
FROM docker:27-cli

COPY --from=build /out/thesis /usr/local/bin/thesis
COPY --from=build /out/thesis-oracle-linearizable /usr/local/bin/thesis-oracle-linearizable

# Loopback is the default because it is correct on a host. In this image it is
# almost always wrong, so it is set explicitly to the address Docker Desktop
# publishes the host on. On a Linux engine, override it with the bridge gateway:
#   -e PROTHESIS_PROBE_HOST=172.17.0.1
ENV PROTHESIS_PROBE_HOST=host.docker.internal

# No default project directory. The working directory is the mounted project,
# and it must match the host path (see 1 above).
ENTRYPOINT ["thesis"]
CMD ["--help"]
