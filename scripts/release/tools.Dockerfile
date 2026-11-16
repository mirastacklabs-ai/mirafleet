# Release tools image: the pinned goreleaser OSS image (MIT; it carries the
# Go toolchain, git and nFPM as a library) plus pinned syft, cosign and the standalone nfpm CLI.
# Every FROM is pinned by version tag AND the multi-arch index digest (the checksum). The Go in
# the goreleaser image must match go.mod; scripts/release.sh prints the versions on build.
# Built by `scripts/release.sh snapshot`; no secrets, no network use at run time except Go modules.
FROM docker.io/anchore/syft:v1.57.1@sha256:03215d0e98b661d0ecbc8575dbc14d65be1743b5de5df6410aecb7e80f95b060 AS syft
FROM gcr.io/projectsigstore/cosign:v3.1.4@sha256:0f06154836d2de34f887a5c273506d52ca43edaa1d9e53e6510fa78a48f69c2d AS cosign
FROM docker.io/goreleaser/nfpm:v2.49.0@sha256:9ea46a59c4d055ef7071b22f3520b53e8dad98e18043b2f881b53bd7ffec634e AS nfpm

FROM docker.io/goreleaser/goreleaser:v2.20.2@sha256:5d30bb36091b1a6d275ef450be05ee9c0d8c8b22955efe53dfe0974dd583cdc2
COPY --from=syft /syft /usr/bin/syft
COPY --from=cosign /ko-app/cosign /usr/bin/cosign
COPY --from=nfpm /usr/bin/nfpm /usr/bin/nfpm
LABEL org.mirastacklabs.mirafleet.release=1
ENTRYPOINT []
CMD ["goreleaser", "--version"]
