FROM golang:1.23-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /consul-sync ./cmd/consul-sync

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /consul-sync /consul-sync
USER 65532:65532
ENTRYPOINT ["/consul-sync"]
