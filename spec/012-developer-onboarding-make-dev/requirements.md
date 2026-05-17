# Requirements — Developer Onboarding `make dev` (meshsat-hub v2.0, MESHSAT-184)

Source: `docs/ROADMAP.md` L151 ("Developer onboarding: `make dev` starts Hub + Simulator + all deps, no hardware needed"), `docs/EXECUTION_PLAN.md` §"Task 18". Depends on `spec/010-meshsat-simulator/` for the simulator.

Constitution invariants in scope: Article XII (preserves schema parity — `make dev` runs migrations against both SQLite + MariaDB stub).

A single `make dev` invocation brings up a fully functional Hub on a contributor's laptop: SQLite store + Mosquitto MQTT + Hub binary + 3 simulated devices. Zero hardware, zero external service signups.

## Functional requirements

REQ-1100: The Makefile shall include a `dev` target that brings up a complete development environment in one command.

REQ-1101: When `make dev` is invoked, the system shall start docker-compose with services `meshsat-hub`, `mosquitto`, and optional `tak-server`.

REQ-1102: When `make dev` finishes initial bring-up, the Hub shall be reachable at `http://localhost:6070`.

REQ-1103: When `make dev` finishes initial bring-up, the system shall automatically start 3 simulators per spec/010 against `imei=000000000000001`, `000000000000002`, `000000000000003` with patterns `text`, `position`, `sos`.

REQ-1104: When `make dev` is invoked AND `docker compose` is not installed, the system shall print an error explaining the prerequisite AND exit non-zero.

REQ-1105: The Makefile shall include a `dev-down` target that stops + removes the dev environment.

REQ-1106: The Makefile shall include a `dev-logs` target that streams docker-compose logs from all dev services.

REQ-1107: The Makefile shall include a `dev-shell` target that opens a bash shell in the running meshsat-hub container.

REQ-1108: When `make dev` is invoked, the dev Hub shall use `HUB_MODE=standalone` AND `HUB_SIMULATOR_ENABLED=true` AND log at level `debug`.

REQ-1109: The dev environment shall ship with a pre-seeded `dev-tenant` tenant containing default routes per spec/007 so messages flow out of the box.

REQ-1110: When the contributor opens the dev Hub UI in a browser AND navigates to the Dashboard, the contributor shall see live message counters incrementing within 30 seconds.

REQ-1111: The dev environment shall use a Docker named volume `meshsat-dev-data` for SQLite persistence so `make dev-down && make dev` retains state.

REQ-1112: The system shall add a `make dev-clean` target that removes the named volume so contributors can start fresh.

REQ-1113: The dev environment shall NOT require any external service credentials (Twilio, AWS, Cloudloop); every external gateway shall operate in no-op mode by default.

REQ-1114: The system shall publish a `docs/dev-onboarding.md` page documenting the `make dev` workflow, troubleshooting for common docker/compose issues, and the URL list for every dev service (Hub UI on localhost:6070, Mosquitto on localhost:1883, optional TAK server).

REQ-1115: The `make dev` flow shall complete (Hub reachable + simulators running) within 60 seconds on a developer laptop with cold docker images cached locally.
