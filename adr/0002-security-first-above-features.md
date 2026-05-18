# 2. Security is #1

Date: 2026-05-18 (codifying Article II)

## Status
Accepted

## Context
meshsat-hub is internet-exposed multi-tenant SaaS. Compromise = cross-tenant data leak + potential ESP32 emergency-path interference.

## Decision
Security wins every priority tie. CI gates HIGH-severity findings. OWASP ZAP every v0.X release.

## Consequences
Slower feature delivery; fewer incidents.
