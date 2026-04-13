FROM golang:1.26-alpine AS build
RUN apk add --no-cache git
WORKDIR /src
RUN git clone https://github.com/codemodify/simple-http-router.git .
RUN go build -trimpath -ldflags="-s -w" -o /simple-http-router .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=build /simple-http-router /usr/local/bin/simple-http-router

# mount points for config and ACME cert cache
VOLUME ["/etc/simple-http-router", "/var/lib/simple-http-router/cert-cache"]

EXPOSE 80 443
ENTRYPOINT ["simple-http-router"]
CMD ["-config", "/etc/simple-http-router/config.json"]
