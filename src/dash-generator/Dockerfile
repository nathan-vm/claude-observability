# Stands in for "the server" in local dev: everywhere else, a user's machine
# only ever sends OTel — dashboard generation happens here, not on their
# laptop. See docker-compose.yaml's dash-generator service and README.md.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -o /out/claude-observability-dash-generator ./cmd/dash-generator

FROM alpine:latest
RUN apk add --no-cache ca-certificates
COPY --from=build /out/claude-observability-dash-generator /usr/local/bin/claude-observability-dash-generator
WORKDIR /app
ENTRYPOINT ["claude-observability-dash-generator"]
