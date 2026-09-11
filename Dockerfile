FROM golang:1.26 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build \
    -o /out/rate-limiter-proxy .

# static-debian12 carries no shell and no package manager. 
# It ships with CA certificates and a predefined Non-root user.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/rate-limiter-proxy /rate-limiter-proxy

EXPOSE 15001 15090

# The :nonroot tag runs as uid 65532, so the image satisfies runAsNonRoot.
USER nonroot:nonroot

ENTRYPOINT ["/rate-limiter-proxy"]
