#!/usr/bin/env python3
"""Stop a health check from writing a stack trace into every tenant's log (MESHSAT-1089).

THE BUG THIS FIXES. `opentakserver/eud_handler/EudHandlerSSL.py` ends setup()
with:

    except BaseException as e:
        self.logger.warning("Failed to do handshake: {}".format(e))
        self.logger.error(traceback.format_exc())
        self.close_connection()

Three problems compound there, and the readiness probe hits all three every ten
seconds for as long as the instance lives:

 1. A Kubernetes TCPSocket probe opens the CoT port and closes it without ever
    sending a ClientHello. `do_handshake()` therefore raises `SSLEOFError`. That
    is not a fault -- nobody was refused, the peer simply went away -- but it is
    logged at WARNING and then again as a full traceback at ERROR.

 2. `close_connection()` publishes a disconnect to RabbitMQ. At this point in
    setup() `self.rabbit_channel` is still None, so it raises
    `AttributeError: 'NoneType' object has no attribute 'basic_publish'` -- an
    unhandled exception INSIDE the error handler, which socketserver then prints
    as a second, longer traceback.

 3. `except BaseException` swallows the distinction between "the peer hung up"
    and "something is genuinely wrong", so both look identical in the log.

Measured on the first real instance: roughly twenty lines every ten seconds, per
tenant, for ever. At the planned 50-70 instances that is a lot of noise, it
buries any real error on a brand-new path, and a 107 GB log has already filled a
root filesystem in this estate once (Incident 20).

WHY NOT JUST CHANGE THE PROBE. The probe earns its keep: without it Kubernetes
counts a container ready the moment it is running, and `ots-eud` reported ready
between crash loops while the CoT port refused every connection -- render.go says
so in its own comment. Removing or slowing it trades a log problem for a
correctness one. The defect is the unhandled exception in the error handler, and
that is what this patches.

WHAT IT PRESERVES. A handshake that fails for a REAL reason still logs a warning
and a traceback, exactly as before. When a rabbit channel does exist the original
close_connection() still runs, so the disconnect notification is unchanged. Only
the case this patch exists for -- the peer vanished before saying anything, with
no channel to publish on -- becomes quiet.

It also sets `self.shutdown` on the instance so `handle()` returns immediately
instead of looping on a socket that never completed a handshake. That shadows the
class attribute, so it affects this connection and no other.

WHY IT FAILS LOUDLY. Same contract as patch-icon-fetch.py: the anchor is
byte-exact (CRLF included -- these files have Windows line endings), it must match
EXACTLY ONCE, and anything else exits non-zero and fails the image build. A patch
that silently no-ops when upstream moves the text would quietly restore the flood.
Re-running on an already-patched file is a no-op.

Usage: patch-eud-handshake.py [EudHandlerSSL.py to patch]

The optional argument exists so this can be PROVEN against the real file outside a
container: the image is about 1 GB and must not be built on the shared runner to
test a text substitution. The Dockerfile never passes it.
"""

import glob
import pathlib
import sys

MARKER = b"MESHSAT-1089"

# Byte-exact, CRLF, as it appears in opentakserver 1.7.13. Leading indent is eight
# spaces: `except` sits inside setup(), inside the class body.
ANCHOR = (
    b'        except BaseException as e:\r\n'
    b'            self.logger.warning("Failed to do handshake: {}".format(e))\r\n'
    b'            self.logger.error(traceback.format_exc())\r\n'
    b'            self.close_connection()\r\n'
)

REPLACEMENT = (
    b'        except BaseException as e:\r\n'
    b'            # MESHSAT-1089: a Kubernetes TCPSocket readiness probe opens this\r\n'
    b'            # port and closes it without a ClientHello, so do_handshake raises\r\n'
    b'            # SSLEOFError every probe period. That is the peer going away, not\r\n'
    b'            # a refusal, and it was being logged as a warning AND a traceback --\r\n'
    b'            # then close_connection raised AttributeError because rabbit_channel\r\n'
    b'            # is still None here, giving a second traceback. Roughly twenty lines\r\n'
    b'            # every ten seconds, per tenant, for ever.\r\n'
    b'            import ssl as _meshsat_ssl\r\n'
    b'            _meshsat_gone = isinstance(e, (\r\n'
    b'                _meshsat_ssl.SSLEOFError,\r\n'
    b'                _meshsat_ssl.SSLZeroReturnError,\r\n'
    b'                ConnectionResetError,\r\n'
    b'                BrokenPipeError,\r\n'
    b'                TimeoutError,\r\n'
    b'            ))\r\n'
    b'            if _meshsat_gone:\r\n'
    b'                self.logger.debug("Handshake abandoned by peer: {}".format(e))\r\n'
    b'            else:\r\n'
    b'                # A REAL failure is still as loud as it ever was.\r\n'
    b'                self.logger.warning("Failed to do handshake: {}".format(e))\r\n'
    b'                self.logger.error(traceback.format_exc())\r\n'
    b'            # handle() loops on `while not self.shutdown`; setting it on the\r\n'
    b'            # instance shadows the class attribute, so this connection stops and\r\n'
    b'            # no other is affected.\r\n'
    b'            self.shutdown = True\r\n'
    b'            if getattr(self, "rabbit_channel", None) is None:\r\n'
    b'                try:\r\n'
    b'                    self.request.close()\r\n'
    b'                except BaseException:\r\n'
    b'                    pass\r\n'
    b'            else:\r\n'
    b'                # Unchanged when there IS a channel to publish the disconnect on.\r\n'
    b'                self.close_connection()\r\n'
)

# The SECOND half of this fix, and the one that actually mattered (MESHSAT-1089).
#
# The first version patched only EudHandlerSSL.setup(), on the reasoning that
# setting self.shutdown would make handle() return harmlessly. It does exit the
# loop -- but handle() calls close_connection() UNCONDITIONALLY on the way out,
# after the loop, and that was never read. Result: SSLEOFError tracebacks went to
# zero and the log still carried nine tracebacks per ninety seconds, because
# close_connection dereferences self.rabbit_channel, which is None when setup()
# never got far enough to create one.
#
# close_connection is the right place: it is reached from setup() AND from
# handle(), so guarding it covers both, which is what patching one caller could
# never do. Publishing a disconnect for a connection that never authenticated is
# meaningless anyway -- there is no uid to report.
CLOSE_ANCHOR = (
    b'    def close_connection(self):\r\n'
    b'        self.logger.info("{} disconnected".format(self.client_address[0]))\r\n'
    b'\r\n'
    b'        self.rabbit_channel.basic_publish(\r\n'
)

CLOSE_REPLACEMENT = (
    b'    def close_connection(self):\r\n'
    b'        # MESHSAT-1089: reached from setup() and from the tail of handle().\r\n'
    b'        # When a peer goes away before the TLS handshake -- which the readiness\r\n'
    b'        # probe does every ten seconds -- there is no rabbit channel and no uid,\r\n'
    b'        # so this dereferenced None and raised AttributeError INSIDE the error\r\n'
    b'        # path, turning one aborted probe into a full traceback.\r\n'
    b'        if getattr(self, "rabbit_channel", None) is None:\r\n'
    b'            self.logger.debug(\r\n'
    b'                "{} closed before it was connected".format(self.client_address[0])\r\n'
    b'            )\r\n'
    b'            return\r\n'
    b'        self.logger.info("{} disconnected".format(self.client_address[0]))\r\n'
    b'\r\n'
    b'        self.rabbit_channel.basic_publish(\r\n'
)

HANDLER_GLOB = "/opt/ots/lib/python*/site-packages/opentakserver/eud_handler/EudHandler.py"
VENV_GLOB = "/opt/ots/lib/python*/site-packages/opentakserver/eud_handler/EudHandlerSSL.py"


def die(msg):
    print(f"patch-eud-handshake: FATAL {msg}", file=sys.stderr)
    return 1


def main() -> int:
    if len(sys.argv) > 2:
        return die("usage: patch-eud-handshake.py [EudHandlerSSL.py]")

    if len(sys.argv) == 2:
        path = pathlib.Path(sys.argv[1])
        if not path.is_file():
            return die(f"{path} is not a file")
    else:
        found = glob.glob(VENV_GLOB)
        if len(found) != 1:
            return die(
                f"expected exactly one EudHandlerSSL.py in the venv, found {found}.\n"
                "The venv layout changed; this patch cannot guess which file to edit."
            )
        path = pathlib.Path(found[0])

    raw = path.read_bytes()

    if MARKER in raw:
        print(f"patch-eud-handshake: {path} is already patched, nothing to do")
        return 0

    n = raw.count(ANCHOR)
    if n != 1:
        return die(
            f"the handshake-failure anchor matched {n} times in {path}, expected exactly 1.\n"
            "\n"
            "OpenTAKServer changed the text this patch rewrites. That is a BUILD\n"
            "FAILURE on purpose: without the patch, every tenant's instance writes\n"
            "about twenty log lines every ten seconds, for ever, in response to its\n"
            "own readiness probe -- and a 107 GB log has already filled a root\n"
            "filesystem in this estate once.\n"
            "\n"
            "Re-read setup() in EudHandlerSSL.py and update ANCHOR and REPLACEMENT\n"
            "together. Do not relax the match to make the build pass."
        )

    path.write_bytes(raw.replace(ANCHOR, REPLACEMENT, 1))
    print(f"patch-eud-handshake: patched {path}")

    return patch_close_connection(path)


def patch_close_connection(ssl_path: pathlib.Path) -> int:
    """Guard close_connection, which both setup() and handle() reach."""
    found = glob.glob(HANDLER_GLOB)
    if len(found) == 1:
        path = pathlib.Path(found[0])
    else:
        # Testing mode: EudHandler.py sits beside the file we were handed.
        path = ssl_path.parent / "EudHandler.py"
        if not path.is_file():
            return die(
                f"expected exactly one EudHandler.py in the venv, found {found}, "
                f"and none beside {ssl_path}."
            )

    raw = path.read_bytes()
    if MARKER in raw:
        print(f"patch-eud-handshake: {path} is already patched, nothing to do")
        return 0

    n = raw.count(CLOSE_ANCHOR)
    if n != 1:
        return die(
            f"the close_connection anchor matched {n} times in {path}, expected exactly 1.\n"
            "\n"
            "Without this guard an aborted connection still produces a full traceback\n"
            "from the tail of handle(), which calls close_connection unconditionally --\n"
            "measured at nine per ninety seconds on a live instance even with the\n"
            "setup() half of this patch applied."
        )

    path.write_bytes(raw.replace(CLOSE_ANCHOR, CLOSE_REPLACEMENT, 1))
    print(f"patch-eud-handshake: patched {path}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
