# Two stages: the first compiles the program in the Go image, the second
# is an empty image (scratch) that receives only the program, the default
# template and the CA certificates. No shell, no package manager.

FROM golang:1.26 AS build

WORKDIR /src
# The modules first, as a layer of their own: it is rebuilt only when
# go.mod or go.sum change.
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG VERSION=UNKNOWN
ARG COMMIT=UNKNOWN
ARG BRANCH=UNKNOWN
ARG BUILD_DATE=UNKNOWN
# CGO_ENABLED=0: a static binary that needs no C library. timetzdata: the
# time zone database is built into the binary, so the image needs none.
RUN CGO_ENABLED=0 go build -trimpath -tags timetzdata \
      -ldflags "-s -w \
        -X github.com/aursu/prometheus-msteams/pkg/version.VERSION=${VERSION} \
        -X github.com/aursu/prometheus-msteams/pkg/version.COMMIT=${COMMIT} \
        -X github.com/aursu/prometheus-msteams/pkg/version.BRANCH=${BRANCH} \
        -X github.com/aursu/prometheus-msteams/pkg/version.BUILDDATE=${BUILD_DATE}" \
      -o /out/prometheus-msteams ./cmd/server

FROM scratch

LABEL org.opencontainers.image.title="prometheus-msteams" \
      org.opencontainers.image.description="Sends Prometheus Alertmanager notifications to Microsoft Teams channels as Adaptive Cards, through Teams Workflows." \
      org.opencontainers.image.licenses="MIT"

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/prometheus-msteams /promteams
COPY default-message-workflow-card.tmpl /default-message-workflow-card.tmpl

# Not root. 65532 is the conventional "nonroot" id of minimal images; scratch
# has no user database, so the number is given directly.
USER 65532:65532
WORKDIR /
EXPOSE 2000

ENTRYPOINT ["/promteams"]
