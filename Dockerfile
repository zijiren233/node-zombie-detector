FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS builder

WORKDIR /detector

COPY ./ /detector

ARG GOARCH

RUN if [ -z "$GOARCH" ]; then echo "GOARCH is not set"; exit 1; fi

RUN GOARCH=$GOARCH go build -trimpath -tags "jsoniter" -ldflags "-s -w" -o detector

FROM alpine:latest

RUN mkdir -p /detector

WORKDIR /detector

RUN apk add --no-cache ca-certificates tzdata && \
    rm -rf /var/cache/apk/*

COPY --from=builder /detector/detector /usr/local/bin/detector

ENTRYPOINT ["detector"]
