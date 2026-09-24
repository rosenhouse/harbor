# Builds Harbor core and jobservice from this fork, on the images of the Harbor release that the e2e chart deploys.
# Core migrates Harbor's database to this fork's schema, so a cluster can't go back to the release's core.
# The build context is the repository root.
ARG harbor_image_version

FROM golang:1.26.4 AS build
WORKDIR /harbor/src
COPY src/go.mod src/go.sum ./
RUN --mount=type=secret,id=ca-bundle,required=false --mount=type=cache,target=/go/pkg/mod \
    export SSL_CERT_FILE=$(test -f /run/secrets/ca-bundle && echo /run/secrets/ca-bundle || echo /etc/ssl/certs/ca-certificates.crt) && \
    go install github.com/go-swagger/go-swagger/cmd/swagger@v0.33.1 && \
    go mod download
COPY api /harbor/api
COPY tools/swagger /harbor/tools/swagger
COPY src ./
RUN rm -rf server/v2.0/models server/v2.0/restapi && swagger generate server -q -f ../api/v2.0/swagger.yaml -A harbor --target server/v2.0 \
    --template-dir=../tools/swagger/templates --exclude-main \
    --additional-initialism=CVE --additional-initialism=GC --additional-initialism=OIDC
COPY VERSION /harbor/
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -buildvcs=false -ldflags "-w -s -X github.com/goharbor/harbor/src/pkg/version.ReleaseVersion=$(cat ../VERSION)" -o /out/harbor_core ./core && \
    go build -buildvcs=false -ldflags "-w -s" -o /out/harbor_jobservice ./jobservice

FROM goharbor/harbor-core:${harbor_image_version} AS core
COPY --from=build /out/harbor_core /harbor/
COPY make/migrations /harbor/migrations
COPY src/core/views /harbor/views

FROM goharbor/harbor-jobservice:${harbor_image_version} AS jobservice
COPY --from=build /out/harbor_jobservice /harbor/
