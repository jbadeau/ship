# ship CI image: the ship CLI plus git (ledger history) and shell tooling.
# Binaries are built on the host (make image) to keep the image build offline-friendly.
FROM alpine:3.22
RUN apk add --no-cache git bash curl jq ca-certificates
ARG TARGETARCH
ARG KUBECTL_VERSION=v1.33.1
RUN curl -fsSL -o /usr/local/bin/kubectl https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl \
 && chmod +x /usr/local/bin/kubectl
COPY dist/linux-${TARGETARCH}/ /usr/local/bin/
ENTRYPOINT []
