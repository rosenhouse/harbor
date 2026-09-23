FROM golang:1.26.4 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=secret,id=ca-bundle,required=false \
    SSL_CERT_FILE=$(test -f /run/secrets/ca-bundle && echo /run/secrets/ca-bundle || echo /etc/ssl/certs/ca-certificates.crt) \
    go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /harbor-apiserver ./cmd/harbor-apiserver

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /harbor-apiserver /harbor-apiserver
ENTRYPOINT ["/harbor-apiserver"]
