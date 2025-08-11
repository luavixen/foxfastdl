FROM golang:1.24-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /build

COPY go.mod go.sum main.go ./
RUN go mod download

RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags='-w -s -extldflags "-static"' -o foxfastdl .

FROM scratch

COPY --from=builder /build/foxfastdl /foxfastdl

USER 65534

EXPOSE 8080

ENTRYPOINT ["/foxfastdl"]
