FROM --platform=$BUILDPLATFORM golang:1.26-alpine@sha256:30b1e546a9c7fa3b747c2bdba3a0c7bdf3a02ebb4c5b8405d2f948d7e8aac4ec AS build

ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ARG VERSION=0.1.0

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# Cross-compile on the build platform; GOARM comes from the variant (v7 -> 7).
RUN GOARM="${TARGETVARIANT#v}" CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath -ldflags "-s -w -X main.VERSION=${VERSION}" -o /out/tuya-ipc-terminal .

FROM alpine:3.24@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf

RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -H -h /data tuya \
    && mkdir /data && chown tuya:tuya /data

COPY --from=build /out/tuya-ipc-terminal /usr/local/bin/tuya-ipc-terminal

USER tuya
WORKDIR /data
ENTRYPOINT ["tuya-ipc-terminal"]
