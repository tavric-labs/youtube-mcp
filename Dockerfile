FROM --platform=$BUILDPLATFORM golang:1.27.4 AS build
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -trimpath -o /out/youtube-mcp ./cmd/youtube-mcp \
 && mkdir -p /out/media

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chown=65532:65532 /out/youtube-mcp /app/youtube-mcp
# Staging area for uploads, private to the non-root user. Ephemeral unless a volume is mounted here.
COPY --from=build --chown=65532:65532 --chmod=0700 /out/media /var/lib/youtube-mcp/media
ENV YOUTUBE_MCP_MEDIA_DIR=/var/lib/youtube-mcp/media
ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/app/youtube-mcp"]
