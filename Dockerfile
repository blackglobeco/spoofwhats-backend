FROM golang:1.25-alpine AS builder

# modernc.org/sqlite is pure Go — no CGO or gcc needed
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o whats-spoofing .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates

WORKDIR /app
COPY --from=builder /app/whats-spoofing .

EXPOSE 8080
CMD ["./whats-spoofing"]
