FROM golang:1.24-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static binary: no libc, so it can run on a base image with nothing in it.
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -ldflags="-s -w" -o /out/daans-cat .

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/daans-cat /daans-cat

ENV DATA_DIR=/data \
    ADDR=:8080
EXPOSE 8080
VOLUME /data
USER nonroot:nonroot

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD ["/daans-cat", "healthcheck"]

ENTRYPOINT ["/daans-cat"]
