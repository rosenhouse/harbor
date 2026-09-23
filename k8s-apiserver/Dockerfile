FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /harbor-apiserver ./cmd/harbor-apiserver

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /harbor-apiserver /harbor-apiserver
ENTRYPOINT ["/harbor-apiserver"]
