# Cross-compile on the build host instead of emulating the target.
FROM --platform=$BUILDPLATFORM golang:1-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src

RUN apk --no-cache add ca-certificates tzdata

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /app ./cmd/server

FROM scratch

COPY --from=build /app /app
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /usr/share/zoneinfo /usr/share/zoneinfo

ARG VERSION
ENV VERSION=$VERSION
LABEL version=$VERSION
LABEL org.opencontainers.image.source=https://github.com/ConnorsApps/frigate-notifications

ENTRYPOINT [ "/app" ]
