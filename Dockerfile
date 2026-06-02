FROM golang:1.23-alpine AS builder
WORKDIR /build
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /arr-reconciler ./cmd/reconciler

# Alpine (not distroless) for the final stage: the entrypoint needs a shell and
# envsubst (gettext) to render the config from environment variables, so secrets
# stay in the compose .env and are never baked into the image.
FROM alpine:3.20
RUN apk add --no-cache gettext ca-certificates
COPY --from=builder /arr-reconciler /usr/local/bin/arr-reconciler
COPY config.template.json /etc/arr-reconciler/config.template.json
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
