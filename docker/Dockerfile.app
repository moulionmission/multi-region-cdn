# Build Stage
FROM golang:1.22-alpine AS builder

WORKDIR /app

# Copy dependency files
COPY go.mod ./
# If go.sum exists, copy it
COPY go.sum* ./

# Download dependencies (only standard lib and pq/go-redis)
RUN go mod download

# Copy source code
COPY cmd/app/main.go ./main.go

# Build binary
RUN CGO_ENABLED=0 GOOS=linux go build -o app main.go

# Production Stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates

WORKDIR /root/

# Copy built binary from builder stage
COPY --from=builder /app/app .

# Expose server port
EXPOSE 8081

# Command to run
CMD ["./app"]
