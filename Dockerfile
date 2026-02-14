# Build stage
FROM golang:1.24-alpine AS builder
WORKDIR /app

# Copy dependencies files first for caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build arguments with default values
ARG TENANCY="54882f1d-3788-44f9-aed6-19a793c4568f"

# Build the application with ldflags
RUN go build -ldflags "\
    -X 'main.cliSetTenancy=$TENANCY'" \
    -o runtime-api ./cmd/runtime

# Runtime stage
FROM alpine:3.20
WORKDIR /app

# Copy binary from builder
COPY --from=builder /app/runtime-api .

# Install dependencies for healthcheck and SSL
RUN apk add --no-cache ca-certificates curl

# Expose application port
EXPOSE 8080

# Run the application
CMD ["./runtime-api"]
