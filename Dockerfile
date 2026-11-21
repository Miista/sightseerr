# The binary is cross-compiled with Go beforehand (`make dist`) and copied in, so building
# the image never compiles anything and never needs QEMU emulation for the other platform.
# distroless/static has no shell and no package manager: nothing in the image but the
# binary, CA certificates and tzdata.
#
# The data directory is made in a throwaway stage, because the final image has no shell.
# It runs on the build machine's own platform (it only makes a directory).
# Drop the `data` stage and the COPY/WORKDIR/VOLUME lines if the app stores nothing.
FROM --platform=$BUILDPLATFORM busybox:1.37 AS data
RUN mkdir -p /out/app/data && chown 65532:65532 /out/app/data && chmod 1777 /out/app/data

FROM gcr.io/distroless/static-debian12:nonroot

ARG TARGETARCH
COPY dist/linux/${TARGETARCH}/spor /spor

# Runs as the image's unprivileged user (65532), or any user given with --user / `user:`.
# /app/data is world-writable with the sticky bit, like /tmp, so a fresh named volume
# (which copies this directory's permissions) is writable by whoever runs the container.
COPY --from=data /out/ /
WORKDIR /app/data
VOLUME /app/data

ENV SPOR_DATA_DIR=/app/data
EXPOSE 8080

# No shell or curl in the image: the binary checks itself.
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD ["/spor", "healthcheck"]

ENTRYPOINT ["/spor"]
