FROM golang:1.22-alpine AS build
WORKDIR /app
COPY go.mod ./
COPY *.go ./
RUN go mod tidy && CGO_ENABLED=0 go build -o sage-sidecar .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
COPY --from=build /app/sage-sidecar /usr/local/bin/
EXPOSE 5433 9187
ENTRYPOINT ["sage-sidecar"]
