FROM golang:1.23-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /k8s-blkio-limiter ./cmd/k8s-blkio-limiter

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /k8s-blkio-limiter /k8s-blkio-limiter
USER root
ENTRYPOINT ["/k8s-blkio-limiter"]
