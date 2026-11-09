# Compiles natively on the build machine and cross-compiles for the target, so
# multi-arch builds don't need QEMU. CGO is off because the SQLite driver is pure Go.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
RUN apk add --no-cache ca-certificates
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH TARGETVARIANT
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags="-s -w" -o /out/sift .
RUN mkdir -p /out/data

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/sift /sift
# Scratch has no mkdir or chown; a new named volume inherits this ownership.
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENV DB_PATH=/data/rss.db
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/sift"]
