# The build runs on the host platform and cross-compiles: no emulation is
# needed for the arm64 image.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY certs ./certs
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /perepost .

# distroless/static: CA certificates and the binary, no shell
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /perepost /perepost
ENV CONFIG_FILE=/etc/perepost/config.yaml
ENTRYPOINT ["/perepost"]
