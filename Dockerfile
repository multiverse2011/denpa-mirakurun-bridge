FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS build
ARG TARGETOS TARGETARCH TARGETVARIANT VERSION=dev
WORKDIR /src
COPY go.mod *.go ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /bridge . && mkdir /config

FROM scratch
COPY --from=build /bridge /bridge
COPY --from=build --chown=65532:65532 /config /config
ENV HOST=0.0.0.0 CHANNELS_FILE=/config/channels.json
USER 65532:65532
EXPOSE 25252
ENTRYPOINT ["/bridge"]
