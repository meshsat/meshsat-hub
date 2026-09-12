# OpenTAKServer for hosted per-tenant TAK (MESHSAT-1034, spike MESHSAT-1035).
#
# Our own build, not the upstream image. Two reasons:
# - The upstream ghcr image created a CA at image build time, so every user of
#   that image shared one CA key.
# - The upstream per-process images run code from a stale branch.
#
# One image runs all three OTS processes; the tak-operator chooses the command
# for each container (opentakserver, eud_handler --ssl, cot_parser). This image
# never creates a CA. Each tenant's CA is created and held by the operator.
#
# requirements.txt is a hash-locked resolve of requirements.in for CPython 3.13
# on manylinux x86_64. overrides.txt replaces the vulnerable exact versions that
# OTS pins, and drops frida:
#   uv pip compile requirements.in --override overrides.txt --python-version 3.13 \
#     --python-platform x86_64-manylinux_2_28 --generate-hashes -o requirements.txt
# pip installs the lock with --no-deps. Otherwise pip would resolve OTS's own
# exact pins and put the vulnerable versions straight back. The lock is complete,
# so --no-deps loses nothing.
#
# A few dependencies ship only sdists (flask-apscheduler, for one), so the venv
# is built on the full image, which has a compiler, and copied into slim. Both
# stages are trixie so the venv's Python and glibc match.
#
# Built by the package:ots CI job as ghcr.io/meshsat/meshsat-hub-ots:<version>.
FROM python:3.13-trixie AS build
RUN python -m venv /opt/ots
COPY requirements.txt /tmp/requirements.txt
RUN /opt/ots/bin/pip install --no-cache-dir --no-deps --require-hashes -r /tmp/requirements.txt
# Marker icons come from the image, not from the internet at run time
# (MESHSAT-1057). OpenTAKServer fetches a 9.6 MB sqlite archive from GitHub with
# NO timeout on first start; a tenant's instance has no egress and Cilium drops
# rather than rejects, so that hung for 134 seconds on EVERY start, with the API
# not listening for the whole window, and the stall was swallowed by a bare
# `except BaseException`.
#
# Both steps run HERE, in the build stage: this is where there is egress, and
# only /opt/ots is copied into the runtime stage, so that tree is the one way to
# get a file into the image. /opt/ots/share is also outside /var/lib/ots, which
# the operator mounts as an EmptyDir that masks anything the image leaves under
# OTS_DATA_FOLDER -- the reason shipping the archive alone would not have worked.
#
# The fetch is digest-pinned and the patch's anchor is byte-exact; either one
# failing fails the build, which is deliberate. A silent no-op here restores a
# 134-second stall that nobody would think to look for twice.
COPY bundle-iconsets.py patch-icon-fetch.py /tmp/
RUN /opt/ots/bin/python /tmp/bundle-iconsets.py /opt/ots/share/iconsets.sqlite \
    && /opt/ots/bin/python /tmp/patch-icon-fetch.py /opt/ots/share/iconsets.sqlite

FROM python:3.13-slim-trixie
# Debian security updates for the packages the BASE IMAGE brings.
#
# Nothing here installs them: perl-base and libsqlite3-0 arrive with
# python:3.13-slim-trixie. That tag is rebuilt on its own schedule, which is
# slower than advisories are published, so the image drifts into failing its own
# CVE gate with no change on our side. It did on 2026-09-12: 12 findings, 9 HIGH
# and 3 CRITICAL, all in those two packages, all with fixes already in Debian
# (MESHSAT-1068). package:ots scans with --ignore-unfixed --exit-code 1 and is
# allow_failure: false, so that reds every branch pipeline in the repo.
#
# Upgrade rather than pin two versions: a pin is correct for exactly one advisory
# and has to be chased afterwards, and the gate already ignores anything with no
# fix available. Only the runtime stage needs it -- the build stage's packages
# never ship, since only /opt/ots is copied out of it.
#
# This is the first apt-get upgrade in the estate; every other Debian-based
# Dockerfile here only installs. So it is deliberate rather than copied from
# somewhere.
#
# CACHE CAVEAT: this layer has no changing inputs, so a runner with a warm build
# cache can reuse it and ship stale packages anyway. If the same two packages
# appear again, the build cache is the reason -- build this job with
# --pull --no-cache rather than editing the line below.
#
# DEBIAN_FRONTEND is exported inside the layer rather than set with ENV, so it
# does not persist into the shipped image and change apt's behaviour for anybody
# who later execs into the container.
RUN export DEBIAN_FRONTEND=noninteractive \
    && apt-get update \
    && apt-get upgrade -y \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*
ENV PATH=/opt/ots/bin:$PATH \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    OTS_DATA_FOLDER=/var/lib/ots
COPY --from=build /opt/ots /opt/ots
# pip is only needed to build the image; nothing is installed at runtime (OTS
# plugins are disabled). pip 26.2.1 vendors msgpack 1.1.2 and setuptools 70.3.0
# (pip/_vendor/vendor.txt), which trivy flags. So remove pip from the venv and
# from the base image, along with ensurepip's bundled wheel. The base
# interpreter is named by its full path because PATH puts the venv first, and
# the venv's pip is already gone by then.
RUN /opt/ots/bin/python -m pip uninstall --yes pip \
    && /usr/local/bin/python3 -m pip uninstall --yes pip \
    && rm -rf /usr/local/lib/python3.13/ensurepip \
    && useradd --system --uid 10001 --user-group --home-dir /var/lib/ots --shell /usr/sbin/nologin ots \
    && mkdir -p /var/lib/ots \
    && chown 10001:10001 /var/lib/ots
USER 10001:10001
WORKDIR /var/lib/ots
CMD ["opentakserver"]
