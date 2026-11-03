FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/santa-api ./cmd/server
FROM alpine:3.21
RUN apk add --no-cache ca-certificates && adduser -D -H -u 10001 app
USER app
COPY --from=build /out/santa-api /santa-api
EXPOSE 8080
ENTRYPOINT ["/santa-api"]
