FROM golang:1.24 AS builder

LABEL authors="moyu114"

WORKDIR /app

COPY go.* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -o unichatgo

# Use debian 12 slim
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y ca-certificates && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=builder /app/unichatgo .
# For China mainland config.json.example_cn
# For Global config.json.example
COPY config.json ./config.json
RUN mkdir -p /app/data
RUN mkdir -p /app/data/mysql

ENV UNICHATGO_CONFIG=/app/config.json
EXPOSE 8090
CMD ["./unichatgo"]
