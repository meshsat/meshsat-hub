# stunnel for the Reticulum leg (MESHSAT-930). Built and pushed by the
# package:stunnel CI job as ghcr.io/meshsat/meshsat-hub-stunnel:3.21 (tag = the
# alpine base); pinned in the root k8s/kustomization.yaml images: block.
FROM alpine:3.21
RUN apk add --no-cache stunnel
USER 65532:65532
ENTRYPOINT ["stunnel", "/etc/stunnel/stunnel.conf"]
