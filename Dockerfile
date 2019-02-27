# Build Geth in a stock Go builder container
FROM golang:1.11-alpine as builder

RUN apk add --no-cache make gcc musl-dev linux-headers

ADD . /go-ethereum
RUN cd /go-ethereum && make all

# Pull Geth into a second stage deploy alpine container
FROM alpine:latest

RUN apk add --no-cache ca-certificates
COPY --from=builder /go-ethereum/build/bin/geth /usr/local/bin/
COPY --from=builder /go-ethereum/build/bin/bootnode /usr/local/bin/
COPY docker.operator.pls.sh /usr/local/bin
COPY docker.user.pls.sh /usr/local/bin
COPY docker.bootnode.sh /usr/local/bin
COPY test-rpc.sh /usr/local/bin
COPY boot.key /usr/local/bin
WORKDIR /usr/local/bin
