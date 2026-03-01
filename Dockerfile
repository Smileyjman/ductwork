# Build stage
FROM golang:1.23-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o ductwork ./cmd/ductwork

# Runtime stage
FROM alpine:3.19

# bash and curl are needed for the agent's bash tool and HTTP operations
RUN apk add --no-cache bash curl

WORKDIR /app
COPY --from=builder /app/ductwork .

ENTRYPOINT ["./ductwork"]
