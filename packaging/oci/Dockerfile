# mirafleet OCI image (docs/M6.md section 2.7). FROM scratch, only COPY lines: buildx builds
# every architecture without QEMU. Not distroless: that ships ca-certificates (MPL-2.0), which
# may not be bundled. No shell, no CA bundle, no certificate or key: the GUI's self-signed
# certificate is generated on first start into the data volume.
#
# Build context layout (goreleaser's dockers_v2 layout; scripts/release/imagetest.sh makes the
# same by hand):
#   <os>/<arch>/mirafleet     the static binary, one directory per TARGETPLATFORM (linux/amd64, ...)
#   LICENSE NOTICE THIRD_PARTY_LICENSES.txt
FROM scratch

ARG TARGETPLATFORM
ARG VERSION=dev
ARG REVISION=unknown

LABEL org.opencontainers.image.title="mirafleet" \
      org.opencontainers.image.description="Claude Code orchestrates fleets of servers through a central server" \
      org.opencontainers.image.source="https://github.com/mirastacklabs-ai/mirafleet" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"

COPY ${TARGETPLATFORM}/mirafleet /usr/bin/mirafleet
COPY LICENSE NOTICE THIRD_PARTY_LICENSES.txt /usr/share/doc/mirafleet/

# The data directory, owned by the non-root user so a new named volume inherits that owner.
COPY --chown=65532:65532 <<EOT /var/lib/mirafleet/.keep
EOT

USER 65532:65532
ENV HOME=/var/lib/mirafleet
VOLUME /var/lib/mirafleet
EXPOSE 2222 8443
ENTRYPOINT ["/usr/bin/mirafleet"]
CMD ["central","run","-data","/var/lib/mirafleet/central","-listen","0.0.0.0:2222"]
