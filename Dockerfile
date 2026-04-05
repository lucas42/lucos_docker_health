FROM golang:1.26 AS builder

WORKDIR /go/src/lucos_docker_health

COPY go.mod go.sum ./
RUN go mod download

COPY main.go .
RUN CGO_ENABLED=0 go build -o lucos_docker_health .

FROM gcr.io/distroless/static-debian12

COPY --from=builder /go/src/lucos_docker_health/lucos_docker_health /lucos_docker_health

HEALTHCHECK CMD ["/lucos_docker_health", "--healthcheck"]

CMD ["/lucos_docker_health"]
