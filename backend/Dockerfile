FROM golang:1.24 AS builder

LABEL authors="moyu114"

WORKDIR /app

COPY go.* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build -o unichatgo

# Use debian 12 slim
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y ca-certificates && rm -rf /var/lib/apt/lists/* \
    && useradd -m -u 10001 unichat

WORKDIR /app
COPY --from=builder /app/unichatgo .
# For China mainland
#COPY config.json.example_cn ./config.json
# For Global
COPY config.json.example ./config.json
RUN mkdir -p /app/data && chown -R unichat:unichat /app
USER unichat

ENV UNICHATGO_CONFIG=/app/config.json
EXPOSE 8090
CMD ["./unichatgo"]
