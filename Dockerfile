FROM golang:latest as builder

ENV GOPATH=$PWD
ENV CGO_ENABLED=0

COPY . .

RUN export GO111MODULE=on && export GOPROXY=https://goproxy.cn

RUN go build

RUN echo "nobody:x:65534:65534:nobody:/:/sbin/nologin" > passwd

FROM scratch

COPY --from=builder /go/passwd /etc/passwd

COPY --from=builder /go/mq-interceptor ./mq-interceptor

USER 65534

ENTRYPOINT [ "/mq-interceptor" ]
