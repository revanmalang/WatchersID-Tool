ARG GO_VERSION=1.17
ARG NODE_VERSION=16.13
ARG ALPINE_VERSION=3.15

FROM node:${NODE_VERSION}-alpine AS node-builder
WORKDIR /app
COPY admin/package.json admin/yarn.lock ./
RUN yarn install --frozen-lockfile
COPY admin/ .
ENV NEXT_TELEMETRY_DISABLED=1
RUN yarn run export

FROM golang:${GO_VERSION}-alpine AS go-builder
ARG WATCHERSID_VERSION=0.0.0
ENV CGO_ENABLED=0
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY pkg ./pkg
COPY --from=node-builder /app/dist ./cmd/watchersid/admin
RUN go build -ldflags="-s -w -X main.version=${WATCHERSID_VERSION}" ./cmd/watchersid

FROM alpine:${ALPINE_VERSION}
WORKDIR /app
COPY --from=go-builder /app/watchersid .

ENTRYPOINT ["./watchersid"]

EXPOSE 8080