# Agent Note: Workbench runtime connection address

Status: implemented

English | [中文](2026-09-05-workbench-runtime-connection-address.zh.md)

## Problem

Workbench returned only a one-time runtime token after node creation and rendered a connection command containing a service-address placeholder. The user had to discover and substitute an address even though the Host already knew the Weave API endpoint and local network interfaces.

## Decision

The Workbench Host now resolves one concrete runtime server URL when it starts. An explicit `WEAVE_RUNTIME_SERVER_URL` wins and represents the deployment's public or otherwise routable URL. Without that setting, an already routable `WEAVE_API_URL` remains unchanged. Loopback, wildcard, and container-only single-label hosts fall back to the preferred external IPv4 interface, favoring RFC 1918 addresses and retaining the API protocol, path, and port. Benchmark and link-local addresses are excluded from fallback selection.

The runtime creation response pairs this server URL with the one-time token. The browser validates both values, shell-quotes them, displays the complete command without a placeholder, and provides a direct copy-command action. The raw token remains available during the one-time display for installation flows that require it separately.

Container deployment forwards `WEAVE_RUNTIME_SERVER_URL` into Workbench because a container-only `WEAVE_API_URL` cannot describe an address reachable from another machine. Bare-host installations may leave it empty and use LAN discovery.

## Verification

Focused Host tests cover configured public URLs, routable API URLs, loopback and container-host LAN fallback, and the create response contract. Focused browser tests cover the complete rendered command and clipboard value.

## Alternatives considered

**Ask the browser user for the Weave address.** Rejected because the deployment owns service routing and Workbench can provide a complete command.

**Discover a public IP through an external service.** Rejected because an outbound address does not prove inbound reachability, port forwarding, TLS, or the correct public hostname.

## Consequences

New runtime nodes receive a command they can run directly. Public deployments must configure their routable URL once at deployment level; local bare-host setups use the preferred LAN address automatically. If a host has neither a configured routable URL nor a usable external IPv4 interface, the resolver retains the original API URL so same-machine connection remains possible.
