FROM golang:1.24-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o k8s-monitor ./cmd

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /app/

COPY --from=builder /app/k8s-monitor ./k8s-monitor
COPY configs/pricing-config.json ./configs/pricing-config.json

CMD ["./k8s-monitor"]