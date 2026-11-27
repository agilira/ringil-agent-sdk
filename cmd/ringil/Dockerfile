# Dockerfile: ringil serve in a container
#
#   docker build -t ringil .
#   docker run -d --name ringil -p 127.0.0.1:8765:8765 -v ringil:/data ringil
#   docker exec ringil ringil serve link      # the app's sign-in link
#
# The port is published on the host's loopback only. serve answers the
# host (the container's gateway) and nothing else on the network, with
# its token or the app's session. The state (configuration, conversations, memory, artifacts)
# is in the volume /data, which outlives the container and its updates.

# The base images by digest: a tag can be moved, a digest cannot. Update
# them on purpose (docker pull, then docker inspect --format '{{index
# .RepoDigests 0}}').
ARG GO_IMAGE=golang:1.27-bookworm@sha256:ce59b41415e2524ae99926fe1f37ce48ba8c194dcbd2a56a65436cdcefa2bd34
ARG RUN_IMAGE=debian:bookworm-slim@sha256:e67eeead736ff7bbe258ef088bff683635c561b1fcfefad0806c1df4ab49ce0e

# The SDK at the tag of a release (--build-arg SDK_REF=v0.1.0), else main;
# a local copy with
#   docker build --build-context sdk=../ringil-agent-sdk -t ringil .
FROM ${GO_IMAGE} AS sdkgit
ARG SDK_REF=main
RUN git clone --depth 1 --branch "${SDK_REF}" https://github.com/agilira/ringil-agent-sdk.git /sdk
FROM scratch AS sdk
COPY --from=sdkgit /sdk /

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY --from=sdk / ringil-agent-sdk
COPY . ringil
WORKDIR /src/ringil
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ringil . && mkdir -p /out/data

FROM ${RUN_IMAGE}
# A shell and the usual tools: the agent's bash tool works in here.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates bash git curl \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --uid 10001 --home-dir /data --shell /bin/bash ringil
COPY --from=build /out/ringil /usr/local/bin/ringil
COPY --from=build --chown=10001:10001 /out/data /data
USER ringil
WORKDIR /data
ENV HOME=/data RINGIL_CONTAINER=1
VOLUME /data
EXPOSE 8765
ENTRYPOINT ["/usr/local/bin/ringil"]
CMD ["serve"]
