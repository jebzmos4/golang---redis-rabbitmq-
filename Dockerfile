# build stage
ARG GO_VERSION=1.10.0
ARG ALPINE_VERSION=3.7  
FROM golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS build-stage  
LABEL maintainer="tech@terragonltd.com"
WORKDIR /go/src/bitbucket.org/terragonengineering/mtn-outgoing-message-sender
COPY ./ /go/src/bitbucket.org/terragonengineering/mtn-outgoing-message-sender
RUN apk add --update --no-cache \
        wget \
        curl \
        git \
    && wget "https://github.com/Masterminds/glide/releases/download/v0.12.3/glide-v0.12.3-`go env GOHOSTOS`-`go env GOHOSTARCH`.tar.gz" -O /tmp/glide.tar.gz \
    && mkdir /tmp/glide \
    && tar --directory=/tmp/glide -xvf /tmp/glide.tar.gz \
    && rm -rf /tmp/glide.tar.gz \
    && export PATH=$PATH:/tmp/glide/`go env GOHOSTOS`-`go env GOHOSTARCH` \
    && glide update -v \
    && glide install \
    && CGO_ENABLED=0 GOOS=`go env GOHOSTOS` GOARCH=`go env GOHOSTARCH` go build \
    && go test $(go list ./... | grep -v /vendor/) \
    && apk del wget curl git

# production stage
FROM alpine:${ALPINE_VERSION}
LABEL maintainer="tech@terragonltd.com"
RUN apk --update add ca-certificates
COPY --from=build-stage /go/src/bitbucket.org/terragonengineering/mtn-outgoing-message-sender/mtn-outgoing-message-sender .
ENTRYPOINT ["/mtn-outgoing-message-sender"]