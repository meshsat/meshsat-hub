#!/usr/bin/env python3
"""Let a chat addressed to one crew reach the Hub's connection (MESHSAT-1706).

THE GAP THIS CLOSES. A TAK user who picks one contact and sends a chat puts a
<marti><dest callsign=.../></marti> (ATAK, WinTAK) or <dest uid=.../> (iTAK) in
it. OpenTAKServer 1.7.13 routes such an event ONLY to the `dms` exchange, keyed by
that callsign or uid (cot_parser.route_cot), and binds `dms` keys for a
connection's own uid and callsign only, once, from the first event that carries a
<contact> (EudHandler.parse_device_info). The Hub writes every crew of a tenant --
the mesh nodes behind a kit, the MeshSat phones -- on ONE connection, which
announces itself as the Hub (takhosted/hello.go). So a dispatcher's chat to a
crew has no queue to land in and RabbitMQ drops it without a word. Proven on a
throwaway 1.7.13 server: internal/takhosted/e2e_ots_dm_test.go.

WHAT IT ADDS. One event type the Hub alone may send, `t-x-meshsat-relay`, whose
<detail> lists the crews this connection writes for:

    <relay uid="meshsat-a1b3c3a4" callsign="MESHSAT-c3a4"/>

Each frame is the FULL current set. The handler binds every listed uid and
callsign as a `dms` routing key onto the connection's own uid queue (which the
connection already consumes), unbinds keys a previous frame listed and this one
does not, and records each binding in `bound_queues`, so the existing
unbind_rabbitmq_queues() removes them when the connection closes. The event
itself is never published: not to the firehose, not to cot_parser, not to any
client.

WHY NOT ONE CONNECTION PER CREW. eud_handler is a ForkingTCPServer: each
connection is a process with its own Flask app and RabbitMQ connection, each is
bound to the whole group picture, callsign-named queues are shared round-robin,
and every leader change of the Hub would send an offline for every crew.

WHO MAY SEND IT. Only a connection authenticated as the OpenTAKServer user the
Hub connects as (`meshsat_hub`, or OTS_MESHSAT_RELAY_USER when set), and only
after that connection has identified itself. From anybody else the event is
dropped and logged, never published: a client must not be able to bind another
contact's direct messages to itself. A key equal to this connection's own uid or
callsign is skipped (already bound), and at most 1024 keys are held per
connection.

THREADING. The bindings are made on the RabbitMQ connection's own ioloop thread
(add_callback_threadsafe), not on the handler thread.

WHY IT FAILS LOUDLY. Same contract as patch-eud-handshake.py: the anchor is
byte-exact (CRLF -- upstream's files have Windows line endings), it must match
EXACTLY ONCE, and anything else exits non-zero and fails the image build.
Re-running on a patched file is a no-op.

Usage: patch-relay-dms.py [EudHandler.py to patch]

The optional argument exists so the patch can be proven against the real file
outside a container; the Dockerfile never passes it.
"""

import glob
import pathlib
import sys

MARKER = b"MESHSAT-1706"

# Byte-exact, CRLF, the tail of handle_cot() in opentakserver 1.7.13 and the head
# of publish_cot() after it.
ANCHOR = (
    b"                db.engine.dispose()\r\n"
    b"\r\n"
    b"        self.publish_cot(event)\r\n"
    b"\r\n"
    b"    def publish_cot(self, event):\r\n"
)

REPLACEMENT = (
    b"                db.engine.dispose()\r\n"
    b"\r\n"
    b"        # MESHSAT-1706: the Hub's list of the crews it writes for. Never\r\n"
    b"        # published, whoever sent it.\r\n"
    b'        if event and event.get("type") == "t-x-meshsat-relay":\r\n'
    b"            self._meshsat_relay(event)\r\n"
    b"            return\r\n"
    b"\r\n"
    b"        self.publish_cot(event)\r\n"
    b"\r\n"
    b"    def _meshsat_relay(self, event):\r\n"
    b"        # MESHSAT-1706: bind each listed crew uid and callsign as a dms\r\n"
    b"        # routing key onto this connection's own uid queue, so a chat a TAK\r\n"
    b"        # user addresses to one crew reaches the Hub, which writes for it.\r\n"
    b"        import os as _meshsat_os\r\n"
    b"\r\n"
    b'        allowed = _meshsat_os.environ.get("OTS_MESHSAT_RELAY_USER", "meshsat_hub")\r\n'
    b'        username = getattr(self.user, "username", None)\r\n'
    b"        if not allowed or username != allowed or not self.uid:\r\n"
    b"            self.logger.warning(\r\n"
    b'                "meshsat relay from {} ({}) ignored".format(username, self.uid)\r\n'
    b"            )\r\n"
    b"            return\r\n"
    b"        want = []\r\n"
    b'        detail = event.find("detail")\r\n'
    b"        if detail is not None:\r\n"
    b'            for relay in detail.find_all("relay"):\r\n'
    b'                for key in (relay.get("uid"), relay.get("callsign")):\r\n'
    b"                    if (\r\n"
    b"                        key\r\n"
    b"                        and len(key) <= 200\r\n"
    b"                        and key != self.uid\r\n"
    b"                        and key != self.callsign\r\n"
    b"                        and key not in want\r\n"
    b"                    ):\r\n"
    b"                        want.append(key)\r\n"
    b"        if len(want) > 1024:\r\n"
    b'            self.logger.warning("meshsat relay listed {} keys, keeping 1024".format(len(want)))\r\n'
    b"            want = want[:1024]\r\n"
    b"        if not self.rabbit_channel or not self.rabbit_channel.is_open:\r\n"
    b'            self.logger.error("meshsat relay: RabbitMQ channel is closed, not binding")\r\n'
    b"            return\r\n"
    b'        held = list(getattr(self, "_meshsat_relayed", []))\r\n'
    b"        add = [k for k in want if k not in held]\r\n"
    b"        drop = [k for k in held if k not in want]\r\n"
    b"        self._meshsat_relayed = want\r\n"
    b"        queue = self.uid\r\n"
    b"        channel = self.rabbit_channel\r\n"
    b"\r\n"
    b"        def apply():\r\n"
    b"            for key in drop:\r\n"
    b'                channel.queue_unbind(exchange="dms", queue=queue, routing_key=key)\r\n'
    b"            for key in add:\r\n"
    b'                channel.queue_bind(exchange="dms", queue=queue, routing_key=key)\r\n'
    b"\r\n"
    b"        self.rabbit_connection.ioloop.add_callback_threadsafe(apply)\r\n"
    b"        for key in drop:\r\n"
    b'            bind = {"exchange": "dms", "routing_key": key, "queue": queue}\r\n'
    b"            if bind in self.bound_queues:\r\n"
    b"                self.bound_queues.remove(bind)\r\n"
    b"        for key in add:\r\n"
    b'            bind = {"exchange": "dms", "routing_key": key, "queue": queue}\r\n'
    b"            if bind not in self.bound_queues:\r\n"
    b"                self.bound_queues.append(bind)\r\n"
    b"        self.logger.info(\r\n"
    b'            "meshsat relay: {} keys held, {} bound, {} unbound".format(\r\n'
    b"                len(want), len(add), len(drop)\r\n"
    b"            )\r\n"
    b"        )\r\n"
    b"\r\n"
    b"    def publish_cot(self, event):\r\n"
)

VENV_GLOB = "/opt/ots/lib/python*/site-packages/opentakserver/eud_handler/EudHandler.py"


def die(msg):
    print(f"patch-relay-dms: FATAL {msg}", file=sys.stderr)
    return 1


def main() -> int:
    if len(sys.argv) > 2:
        return die("usage: patch-relay-dms.py [EudHandler.py]")

    if len(sys.argv) == 2:
        path = pathlib.Path(sys.argv[1])
        if not path.is_file():
            return die(f"{path} is not a file")
    else:
        found = glob.glob(VENV_GLOB)
        if len(found) != 1:
            return die(
                f"expected exactly one EudHandler.py in the venv, found {found}.\n"
                "The venv layout changed; this patch cannot guess which file to edit."
            )
        path = pathlib.Path(found[0])

    raw = path.read_bytes()

    if MARKER in raw:
        print(f"patch-relay-dms: {path} is already patched, nothing to do")
        return 0

    n = raw.count(ANCHOR)
    if n != 1:
        return die(
            f"the handle_cot anchor matched {n} times in {path}, expected exactly 1.\n"
            "\n"
            "OpenTAKServer changed the text this patch rewrites. That is a BUILD\n"
            "FAILURE on purpose: without the patch a TAK user's chat to one crew is\n"
            "dropped by RabbitMQ without a word, and the Hub's relay event would be\n"
            "published to every client as an unknown CoT.\n"
            "\n"
            "Re-read handle_cot() and publish_cot() in EudHandler.py and update ANCHOR\n"
            "and REPLACEMENT together. Do not relax the match to make the build pass."
        )

    path.write_bytes(raw.replace(ANCHOR, REPLACEMENT, 1))
    print(f"patch-relay-dms: patched {path}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
