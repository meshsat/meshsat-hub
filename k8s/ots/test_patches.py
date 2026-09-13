#!/usr/bin/env python3
"""Tests for the OpenTAKServer image patches.

# Why this file exists

Nothing tested these. The Dockerfile asserts the patches APPLIED -- the marker is
in the module Python loads -- and that is not the same as asserting they WORK.
The difference is not academic: MESHSAT-1089's first fix passed that assertion
while the instance still wrote nine tracebacks every ninety seconds, because the
patch cured one caller of close_connection and the other caller was never read.
It was caught by measuring production afterwards, which is a poor last line of
defence and not repeatable.

So these tests execute the patched code and assert its behaviour, which is what
would have failed on that incomplete fix.

# Why the fixtures are synthetic

OpenTAKServer is GPL-3.0-or-later; this repository is Apache-2.0. Vendoring its
source as test fixtures would be a licence-compatibility problem, so nothing here
copies an upstream file. Each fixture is assembled from the patch script's OWN
anchor constant -- already in this repo, and only a few lines -- wrapped in
scaffolding written here.

That splits the job cleanly, and both halves already exist:

  - does the anchor still match real upstream?  the image build, which fails
    loudly when upstream moves the text
  - does the patched result behave?             this file

Run: python3 -m unittest discover -s k8s/ots -p 'test_*.py'
"""

import ast
import importlib.util
import pathlib
import py_compile
import subprocess
import sys
import tempfile
import textwrap
import types
import unittest
from unittest.mock import MagicMock

HERE = pathlib.Path(__file__).resolve().parent
HANDSHAKE = HERE / "patch-eud-handshake.py"
SITECUSTOMIZE = HERE / "sitecustomize.py"


def load_patch_module():
    """Import patch-eud-handshake.py for its ANCHOR constants (the name has a dash)."""
    spec = importlib.util.spec_from_file_location("patch_eud_handshake", HANDSHAKE)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


P = load_patch_module()


ICONFETCH = HERE / "patch-icon-fetch.py"


def load_icon_module():
    """Import patch-icon-fetch.py for its ANCHOR and REPLACEMENT (the name has a dash)."""
    spec = importlib.util.spec_from_file_location("patch_icon_fetch", ICONFETCH)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


I = load_icon_module()


def write_app_fixture(d: pathlib.Path, anchor=None):
    """Build a minimal app.py around the real anchor.

    Built from the script's OWN constant, never from a vendored copy of
    app.py: OpenTAKServer is GPL-3.0-or-later and this repository is Apache-2.0.
    CRLF throughout, because app.py is CRLF and the anchor is byte-exact.
    """
    src = (
        b"import os\r\n"
        b"import requests\r\n"
        b"\r\n"
        b"\r\n"
        b"def main():\r\n"
        b"    with app.app_context():\r\n"
        b"        if icons == 0:\r\n"
        b"            try:\r\n"
        + (I.ANCHOR if anchor is None else anchor)
        + b"                logger.info('loaded')\r\n"
        b"            except BaseException as e:\r\n"
        b"                logger.error(e)\r\n"
    )
    path = d / "app.py"
    path.write_bytes(src)
    return path


def run_icon_patch(bundled, app_path):
    return subprocess.run(
        [sys.executable, str(ICONFETCH), str(bundled), str(app_path)],
        capture_output=True, text=True,
    )


def write_fixtures(d: pathlib.Path):
    """Build a minimal EudHandlerSSL.py and EudHandler.py around the real anchors.

    CRLF throughout, because upstream's files are CRLF and the anchors are
    byte-exact -- a fixture with Unix endings would not match, which is itself
    asserted below.
    """
    ssl_src = (
        b"import traceback\r\n"
        b"\r\n"
        b"\r\n"
        b"class EudHandlerSSL:\r\n"
        b"    shutdown = False\r\n"
        b"\r\n"
        b"    def setup(self):\r\n"
        b"        try:\r\n"
        b"            self.request.do_handshake()\r\n"
        + P.ANCHOR
    )
    # close_connection's tail is ours, not upstream's: the anchor covers the
    # opening lines, and everything after it here is scaffolding so the function
    # can be executed.
    handler_src = (
        b"import json\r\n"
        b"\r\n"
        b"\r\n"
        b"class EudHandler:\r\n"
        + P.CLOSE_ANCHOR
        + b'            exchange="cot_parser",\r\n'
        b"            body=json.dumps({}),\r\n"
        b'            routing_key="cot_parser",\r\n'
        b"        )\r\n"
        b"        self.unbind_rabbitmq_queues()\r\n"
    )
    (d / "EudHandlerSSL.py").write_bytes(ssl_src)
    (d / "EudHandler.py").write_bytes(handler_src)
    return d / "EudHandlerSSL.py", d / "EudHandler.py"


def run_patch(ssl_path):
    return subprocess.run(
        [sys.executable, str(HANDSHAKE), str(ssl_path)],
        capture_output=True, text=True,
    )


def extract(path, func_name):
    """Return the source of one function out of a patched file."""
    with open(path, encoding="utf-8", newline="") as fh:
        src = fh.read()
    for node in ast.walk(ast.parse(src)):
        if isinstance(node, ast.FunctionDef) and node.name == func_name:
            return textwrap.dedent(ast.get_source_segment(src, node))
    raise AssertionError(f"{func_name} not found in {path}")


class Recorder:
    """A logger that remembers the level of every call."""

    def __init__(self):
        self.lines = []

    def __getattr__(self, level):
        return lambda msg, *a: self.lines.append((level, str(msg)))

    @property
    def levels(self):
        return [lvl for lvl, _ in self.lines]


class PatchApplication(unittest.TestCase):
    def test_it_applies_to_both_files_and_is_idempotent(self):
        with tempfile.TemporaryDirectory() as d:
            ssl_path, handler_path = write_fixtures(pathlib.Path(d))
            r = run_patch(ssl_path)
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertIn(b"MESHSAT-1089", ssl_path.read_bytes())
            self.assertIn(b"MESHSAT-1089", handler_path.read_bytes(),
                          "close_connection was not patched -- the caller that "
                          "MESHSAT-1089's first fix missed")
            again = run_patch(ssl_path)
            self.assertEqual(again.returncode, 0)
            self.assertIn("already patched", again.stdout)

    def test_the_result_compiles_and_keeps_crlf(self):
        with tempfile.TemporaryDirectory() as d:
            ssl_path, handler_path = write_fixtures(pathlib.Path(d))
            self.assertEqual(run_patch(ssl_path).returncode, 0)
            for p in (ssl_path, handler_path):
                py_compile.compile(str(p), doraise=True)
                raw = p.read_bytes()
                self.assertEqual(raw.count(b"\n") - raw.count(b"\r\n"), 0,
                                 f"{p.name} gained a lone LF; the anchors are byte-exact")

    def test_drift_in_either_anchor_refuses_and_writes_nothing(self):
        for which in ("ssl", "handler"):
            with self.subTest(anchor=which), tempfile.TemporaryDirectory() as d:
                ssl_path, handler_path = write_fixtures(pathlib.Path(d))
                target = ssl_path if which == "ssl" else handler_path
                # Mutate text that is INSIDE the anchor. A first attempt used
                # b"self." and hit a line BEFORE the anchor, so the patch applied
                # cleanly and the test reported a pass it had not earned.
                inside = b"Failed to do handshake" if which == "ssl" else b"disconnected"
                self.assertIn(inside, target.read_bytes())
                target.write_bytes(target.read_bytes().replace(inside, b"MOVED", 1))
                before = {p: p.read_bytes() for p in (ssl_path, handler_path)}
                r = run_patch(ssl_path)
                self.assertNotEqual(r.returncode, 0, "a drifted anchor was accepted")
                # The SSL file is written before the handler is attempted, so only
                # assert that the file whose anchor drifted is untouched.
                self.assertEqual(target.read_bytes(), before[target],
                                 "a refusal still modified the file")

    def test_an_lf_converted_file_is_refused(self):
        with tempfile.TemporaryDirectory() as d:
            ssl_path, _ = write_fixtures(pathlib.Path(d))
            ssl_path.write_bytes(ssl_path.read_bytes().replace(b"\r\n", b"\n"))
            self.assertNotEqual(run_patch(ssl_path).returncode, 0)


class PatchedBehaviour(unittest.TestCase):
    """The half the Dockerfile assertion cannot check."""

    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        d = pathlib.Path(self.dir.name)
        self.ssl_path, self.handler_path = write_fixtures(d)
        self.assertEqual(run_patch(self.ssl_path).returncode, 0)

    def tearDown(self):
        self.dir.cleanup()

    def _close_connection(self):
        ns = {"json": __import__("json")}
        exec(extract(self.handler_path, "close_connection"), ns)
        return ns["close_connection"]

    def _handler(self, channel):
        h = MagicMock()
        h.rabbit_channel = channel
        h.logger = Recorder()
        h.client_address = ("10.2.3.13", 50618)
        h.unbind_rabbitmq_queues = MagicMock()
        return h

    # THE REGRESSION TEST. MESHSAT-1089's first fix cured setup() and left the
    # tail of handle() calling this with no channel, which raised AttributeError
    # inside the error path -- nine tracebacks per ninety seconds, on every
    # tenant, for ever.
    def test_close_connection_returns_early_when_there_is_no_channel(self):
        close = self._close_connection()
        h = self._handler(None)
        close(h)  # must not raise
        self.assertEqual(h.logger.levels, ["debug"],
                         "an aborted connection logged above debug")
        h.unbind_rabbitmq_queues.assert_not_called()

    def test_close_connection_is_unchanged_for_a_real_connection(self):
        close = self._close_connection()
        channel = MagicMock()
        h = self._handler(channel)
        close(h)
        channel.basic_publish.assert_called_once()
        h.unbind_rabbitmq_queues.assert_called_once()
        self.assertEqual(h.logger.levels, ["info"])

    def _setup(self):
        ns = {"traceback": __import__("traceback")}
        exec(extract(self.ssl_path, "setup"), ns)
        return ns["setup"]

    def test_setup_is_quiet_when_the_peer_vanishes(self):
        import ssl as ssl_mod
        setup = self._setup()
        for err in (ssl_mod.SSLEOFError("eof"), ConnectionResetError(),
                    BrokenPipeError(), TimeoutError()):
            with self.subTest(err=type(err).__name__):
                h = self._handler(None)
                h.request.do_handshake.side_effect = err
                setup(h)
                self.assertEqual(h.logger.levels, ["debug"],
                                 "a readiness probe produced more than one debug line")
                self.assertTrue(h.shutdown, "handle() would loop on a dead socket")

    def test_setup_is_still_loud_for_a_real_failure(self):
        setup = self._setup()
        h = self._handler(None)
        h.request.do_handshake.side_effect = ValueError("certificate verify failed")
        setup(h)
        self.assertEqual(h.logger.levels, ["warning", "error"],
                         "a genuine handshake failure was demoted to debug")


class RequestTimeoutDefault(unittest.TestCase):
    """sitecustomize.py -- MESHSAT-1070."""

    def _install(self):
        # A stand-in requests module: this proves the wrapper without a network.
        seen = []

        class Session:
            def request(self, method, url, **kw):
                seen.append(kw.get("timeout", "NONE"))
                return "ok"

        fake = types.ModuleType("requests")
        fake.Session = Session
        sys.modules["requests"] = fake
        # Load by PATH, never by name: Python imports a sitecustomize at
        # interpreter startup, so `import sitecustomize` returns the SYSTEM one
        # and proves nothing. That trap cost a wrong green once already.
        spec = importlib.util.spec_from_file_location("meshsat_sc", SITECUSTOMIZE)
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)
        return Session, seen, spec, mod

    def tearDown(self):
        sys.modules.pop("requests", None)

    def test_a_call_without_a_timeout_gets_the_default(self):
        Session, seen, _, _ = self._install()
        Session().request("GET", "https://example.invalid/")
        self.assertEqual(seen, [(10.0, 60.0)])

    def test_an_explicit_timeout_always_wins(self):
        Session, seen, _, _ = self._install()
        Session().request("GET", "https://example.invalid/", timeout=5)
        Session().request("POST", "https://example.invalid/", timeout=(1, 2))
        self.assertEqual(seen, [5, (1, 2)])

    def test_installing_twice_does_not_wrap_twice(self):
        Session, _, spec, mod = self._install()
        first = Session.request
        spec.loader.exec_module(mod)
        self.assertIs(Session.request, first)


class HttpxTimeoutDefault(unittest.TestCase):
    """sitecustomize.py's httpx half -- MESHSAT-1107.

    httpx already defaults to 5 s, so this is not about an unbounded hang. It is
    about the number being OURS: a library default can move on an upgrade, and if
    it ever became None these calls would silently acquire the failure mode the
    requests half exists to prevent.
    """

    def _install(self):
        built = []

        class Timeout:
            def __init__(self, value):
                self.value = value

            def __eq__(self, other):
                return isinstance(other, Timeout) and other.value == self.value

            def __repr__(self):
                return f"Timeout({self.value})"

        class Client:
            def __init__(self, *args, **kw):
                built.append(kw.get("timeout", "NONE"))

        class AsyncClient(Client):
            pass

        fake = types.ModuleType("httpx")
        fake.Client = Client
        fake.AsyncClient = AsyncClient
        fake.Timeout = Timeout
        sys.modules["httpx"] = fake
        # requests too: sitecustomize installs both, and without a stand-in the
        # requests half would import the real one or bail, neither of which this
        # test wants to depend on.
        sys.modules.setdefault("requests", types.ModuleType("requests"))
        sys.modules["requests"].Session = type("Session", (), {"request": lambda self, *a, **k: None})

        # By PATH, never by name -- `import sitecustomize` returns the SYSTEM one.
        spec = importlib.util.spec_from_file_location("meshsat_sc_httpx", SITECUSTOMIZE)
        mod = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(mod)
        return fake, built, spec, mod

    def tearDown(self):
        sys.modules.pop("httpx", None)
        sys.modules.pop("requests", None)

    def test_a_client_built_without_a_timeout_gets_ours(self):
        httpx, built, _, _ = self._install()
        httpx.Client(http2=True)  # exactly how tak_gov_link_api.py builds it
        self.assertEqual(built, [httpx.Timeout(5.0)])

    def test_the_pinned_value_matches_the_library_default_on_purpose(self):
        """If this ever needs changing, read the docstring first.

        The number is deliberately identical to httpx 0.28.1's own default,
        measured in the running image. Raising it to the generous requests
        numbers would make a hanging tak.gov call fail twelve times slower than
        it does today -- a regression dressed up as a fix.
        """
        spec = importlib.util.spec_from_file_location("meshsat_sc_const", SITECUSTOMIZE)
        mod = importlib.util.module_from_spec(spec)
        sys.modules.setdefault("requests", types.ModuleType("requests"))
        sys.modules["requests"].Session = type("S", (), {"request": lambda self, *a, **k: None})
        spec.loader.exec_module(mod)
        self.assertEqual(mod._HTTPX_TIMEOUT, 5.0)

    def test_an_explicit_timeout_always_wins(self):
        httpx, built, _, _ = self._install()
        httpx.Client(http2=True, timeout=1.5)
        httpx.Client(timeout=None)  # even None: if upstream means it, upstream wins
        self.assertEqual(built, [1.5, None])

    def test_the_async_client_is_covered_too(self):
        httpx, built, _, _ = self._install()
        httpx.AsyncClient()
        self.assertEqual(built, [httpx.Timeout(5.0)])

    def test_installing_twice_does_not_wrap_twice(self):
        httpx, _, spec, mod = self._install()
        first = httpx.Client.__init__
        spec.loader.exec_module(mod)
        self.assertIs(httpx.Client.__init__, first)



class IconPatchApplication(unittest.TestCase):
    """patch-icon-fetch.py -- MESHSAT-1108.

    Its sibling patch-eud-handshake.py has had this coverage since MESHSAT-1089.
    This one rewrites app.py in every tenant's image and had none, so nothing
    proved the loader it writes still works -- and that loader is what decides
    whether an instance spends 134 seconds of every start on a blocked fetch.
    """

    def test_it_applies_once_and_is_idempotent(self):
        with tempfile.TemporaryDirectory() as d:
            d = pathlib.Path(d)
            app = write_app_fixture(d)
            r = run_icon_patch("/opt/ots/share/iconsets.sqlite", app)
            self.assertEqual(r.returncode, 0, r.stderr)
            once = app.read_bytes()
            self.assertEqual(once.count(I.MARKER), 1)
            self.assertNotIn(I.ANCHOR, once)

            r2 = run_icon_patch("/opt/ots/share/iconsets.sqlite", app)
            self.assertEqual(r2.returncode, 0, r2.stderr)
            self.assertEqual(app.read_bytes(), once, "a second run changed the file")
            self.assertIn("already patched", r2.stdout)

    def test_the_result_compiles_and_keeps_crlf(self):
        with tempfile.TemporaryDirectory() as d:
            d = pathlib.Path(d)
            app = write_app_fixture(d)
            self.assertEqual(run_icon_patch("/x/icons.sqlite", app).returncode, 0)
            raw = app.read_bytes()
            self.assertNotIn(b"\n", raw.replace(b"\r\n", b""), "a bare LF crept in")
            py_compile.compile(str(app), doraise=True)

    def test_drift_refuses_and_writes_nothing(self):
        """A silent no-op would restore a 134 s stall nobody would look for twice."""
        with tempfile.TemporaryDirectory() as d:
            d = pathlib.Path(d)
            drifted = I.ANCHOR.replace(b"stream=True", b"stream=False")
            app = write_app_fixture(d, anchor=drifted)
            before = app.read_bytes()
            r = run_icon_patch("/x/icons.sqlite", app)
            self.assertNotEqual(r.returncode, 0, "drift was accepted")
            self.assertEqual(app.read_bytes(), before, "a refused patch still wrote")
            self.assertIn("expected exactly 1", r.stderr)

    def test_an_lf_converted_file_is_refused(self):
        with tempfile.TemporaryDirectory() as d:
            d = pathlib.Path(d)
            app = write_app_fixture(d)
            app.write_bytes(app.read_bytes().replace(b"\r\n", b"\n"))
            before = app.read_bytes()
            r = run_icon_patch("/x/icons.sqlite", app)
            self.assertNotEqual(r.returncode, 0)
            self.assertEqual(app.read_bytes(), before)


class IconLoaderBehaviour(unittest.TestCase):
    """Execute the loader the patch writes, rather than trusting the substitution."""

    def _run(self, bundled, exists):
        body = textwrap.dedent(I.REPLACEMENT.format(bundled=bundled))
        calls = []

        class FakeRequests:
            @staticmethod
            def get(url, **kw):
                calls.append(kw)
                return types.SimpleNamespace(content=b"from-network")

        log = Recorder()
        ns = {
            "os": types.SimpleNamespace(path=types.SimpleNamespace(exists=lambda p: exists)),
            "requests": FakeRequests,
            "logger": log,
            "open": open,
        }
        exec(body, ns)
        return ns["r"], calls, log

    def test_it_reads_the_bundled_archive_when_it_is_there(self):
        with tempfile.TemporaryDirectory() as d:
            archive = pathlib.Path(d) / "iconsets.sqlite"
            archive.write_bytes(b"SQLite format 3\x00PRETEND")
            r, calls, log = self._run(str(archive), exists=True)
            self.assertEqual(r.content, b"SQLite format 3\x00PRETEND")
            self.assertEqual(calls, [], "it went to the network with the archive present")
            self.assertTrue(any("bundled icon sets" in m.lower() for _, m in log.lines))

    def test_the_network_fallback_carries_a_timeout(self):
        """Upstream's call has none; a fallback without one restores the bug."""
        r, calls, log = self._run("/does/not/exist", exists=False)
        self.assertEqual(r.content, b"from-network")
        self.assertEqual(len(calls), 1)
        self.assertIn("timeout", calls[0], "the fallback fetch has no timeout")
        self.assertEqual(calls[0]["timeout"], (5, 30))
        self.assertTrue(any("falling back" in m.lower() for _, m in log.lines))



if __name__ == "__main__":
    unittest.main()
