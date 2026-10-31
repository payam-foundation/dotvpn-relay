# Packs the binary built by goreleaser (dist/dotvpn-relay_linux_<arch>).
#   docker buildx build -f deploy/docker/Dockerfile --platform linux/amd64,linux/arm64 .
# The base is pinned by digest; bump it deliberately, never use a floating tag.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

LABEL org.opencontainers.image.source="https://github.com/payam-foundation/dotvpn-relay" \
      org.opencontainers.image.licenses="PolyForm-Noncommercial-1.0.0" \
      org.opencontainers.image.description="dotvpn relay"

ARG TARGETARCH
COPY dist/dotvpn-relay_linux_${TARGETARCH} /usr/local/bin/dotvpn-relay

# Pre-own the mount point so a fresh named volume is writable by nonroot (65532).
COPY --chown=65532:65532 deploy/docker/data /data

# Only the relay's own identity key and control's public key live here.
VOLUME /data
EXPOSE 443/tcp 53/udp
USER nonroot
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/dotvpn-relay", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/dotvpn-relay"]
CMD ["version"]
