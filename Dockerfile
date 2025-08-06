# Build Geth in a stock Go builder container
FROM golang:1.21-alpine as builder

RUN apk add --no-cache make cmake gcc musl-dev linux-headers git bash build-base libc-dev libstdc++

COPY go.mod /go-ethereum/
COPY go.sum /go-ethereum/
RUN cd /go-ethereum && go mod download

ADD . /go-ethereum

ENV CGO_CFLAGS="-O -D__BLST_PORTABLE__"
ENV CGO_CFLAGS_ALLOW="-O -D__BLST_PORTABLE__"
RUN cd /go-ethereum && go run build/ci.go install -static ./cmd/geth

# Pull Geth into a second stage deploy alpine container
FROM alpine:latest

RUN apk add --no-cache ca-certificates curl jq tini
COPY --from=builder /go-ethereum/build/bin/geth /usr/local/bin/

EXPOSE 6060 8545 8546 8547 30303 30303/udp
ENTRYPOINT ["geth"]
