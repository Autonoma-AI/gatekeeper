# Build a static binary, then ship it on distroless (nonroot). Final image ~15-25MB.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

WORKDIR /src

# Cache module downloads across source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO disabled so the binary is fully static and runs on distroless/static.
# Cross-compiled natively via TARGETARCH (buildx-provided) instead of emulating the target arch.
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/gatekeeper ./cmd/gatekeeper

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gatekeeper /gatekeeper
# Non-root cannot bind <1024, so Gatekeeper listens on 8080.
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/gatekeeper"]
