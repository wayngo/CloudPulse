# syntax=docker/dockerfile:1.7

# ----------------------------------------------------------------------------
# Build stage: compile a fully-static binary so the runtime image can be
# distroless / scratch and stay well under 50MB.
# ----------------------------------------------------------------------------
FROM golang:1.25-alpine AS builder

# git is needed for VCS stamping of build info; tzdata gives us zoneinfo bundled
# into the final image via /usr/share/zoneinfo if we ever need it.
RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Cache module downloads in a dedicated layer so source-only edits skip them.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# CGO is disabled so the binary is fully static and runnable on distroless/static.
# -s -w strips symbols and the DWARF table, -trimpath removes local paths.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/cloudpulse ./

# ----------------------------------------------------------------------------
# Runtime stage: distroless/static gives us a non-root user, ca-certs for
# HTTPS pings, and tzdata, while staying around ~2MB on top of our binary.
# ----------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /

COPY --from=builder /out/cloudpulse /cloudpulse

EXPOSE 8080
USER nonroot:nonroot

ENTRYPOINT ["/cloudpulse"]
