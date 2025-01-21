FROM golang:1.21-alpine

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN go build -o watershed ./cmd/local

# Default to single run mode, but allow override via command line
ENTRYPOINT ["./watershed"]
CMD [] 