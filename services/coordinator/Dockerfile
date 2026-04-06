FROM golang:1.26-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /coordinator ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /coordinator /coordinator
COPY --from=build /app/migrations /migrations
ENV GOOSE_MIGRATION_DIR=/migrations
USER 1000
ENTRYPOINT ["/coordinator"]
