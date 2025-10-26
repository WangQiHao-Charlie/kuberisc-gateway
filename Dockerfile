FROM --platform=$BUILDPLATFORM golang:1.24 AS build
WORKDIR /src

# Leverage Go module cache
COPY go.mod go.sum ./
RUN go mod download

# Copy rest of the sources
COPY . ./

# Build for target platform
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/gateway /gateway
USER nonroot:nonroot
ENTRYPOINT ["/gateway"]
