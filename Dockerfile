# Lean production image: prebuilt static Go binary + static ffmpeg on distroless.
# Build with `make docker_build` (compiles bin/sevi first, then this image).
# Config baked in: config.prod.json (edit secrets.name first).

# Single static ffmpeg binary with libmp3lame and libopus; no shared libraries.
FROM mwader/static-ffmpeg:7.1 AS ffmpeg

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=ffmpeg /ffmpeg /usr/local/bin/ffmpeg
COPY bin/sevi /app/sevi
COPY config.prod.json /app/config.json
WORKDIR /app
ENTRYPOINT ["/app/sevi", "-config", "/app/config.json"]
