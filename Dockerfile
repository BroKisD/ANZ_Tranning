# ========== BUILD ==========
FROM golang:1.26 AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w" \
    -o /app \
    .

# ========== RUNTIME ==========
# git is needed at runtime: the platform clones the template repo and pushes the
# generated code to the user's repo. ca-certificates for https to GitHub / Google.
FROM debian:12-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --create-home --uid 10001 app

COPY --from=builder /app /app

USER app

EXPOSE 8080

ENTRYPOINT ["/app"]
CMD ["serve"]
