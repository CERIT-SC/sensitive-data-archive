FROM golang:latest as builder

ENV GOPATH=$PWD
ENV CGO_ENABLED=0

COPY . .

RUN go build

FROM ubuntu:24.04

RUN apt update && apt-get install -y ca-certificates

WORKDIR /

COPY --from=builder /go/mq-interceptor /mq-interceptor
RUN chmod +x /mq-interceptor

USER 1000

ENTRYPOINT [ "/mq-interceptor" ]
