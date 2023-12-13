FROM golang:1.20-alpine

RUN apk add ca-certificates

FROM alpine:3

RUN apk add --no-cache ffmpeg
