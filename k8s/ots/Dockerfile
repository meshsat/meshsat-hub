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

FROM python:3.13-slim-trixie
ENV PATH=/opt/ots/bin:$PATH \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    OTS_DATA_FOLDER=/var/lib/ots
COPY --from=build /opt/ots /opt/ots
# pip is only needed to build the image; nothing is installed at runtime (OTS
# plugins are disabled). pip 26.2.1 vendors msgpack 1.1.2 and setuptools 70.3.0
# (pip/_vendor/vendor.txt), which trivy flags. So remove pip from the venv and
# from the base image, along with ensurepip's bundled wheel.
RUN /opt/ots/bin/python -m pip uninstall --yes pip \
    && python -m pip uninstall --yes pip \
    && rm -rf /usr/local/lib/python3.13/ensurepip \
    && useradd --system --uid 10001 --user-group --home-dir /var/lib/ots --shell /usr/sbin/nologin ots \
    && mkdir -p /var/lib/ots \
    && chown 10001:10001 /var/lib/ots
USER 10001:10001
WORKDIR /var/lib/ots
CMD ["opentakserver"]
