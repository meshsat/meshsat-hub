# The nightly verification runner (MESHSAT-1153): python for the suites, bash
# and curl for the shell ones, and a kubectl that matches the cluster so the
# suites can exec into the Hub and the database pods the way they do from the
# runner. Built and pushed by the package:verify CI job as
# ghcr.io/meshsat/meshsat-hub-verify:<KUBECTL_VERSION>; pinned in the root
# k8s/kustomization.yaml images: block. No apk at run time: the Job runs on a
# read-only root filesystem.
FROM docker.io/library/python:3.13-alpine
ARG KUBECTL_VERSION=v1.36.3
RUN apk add --no-cache bash curl \
 && curl -fsSL "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl" -o /usr/local/bin/kubectl \
 && curl -fsSL "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl.sha256" -o /tmp/kubectl.sha256 \
 && echo "$(cat /tmp/kubectl.sha256)  /usr/local/bin/kubectl" | sha256sum -c - \
 && chmod 0755 /usr/local/bin/kubectl && rm /tmp/kubectl.sha256
USER 65534:65534
ENTRYPOINT ["/bin/bash", "/suites/run.sh"]
