# Agent Note: Workbench container deployment

Status: implemented

English | [中文](2026-09-03-workbench-container-deployment.zh.md)

## Problem

The Workbench profile could run from a developer checkout, but a remote installation had no reproducible image, persistent Harness home, private Weave connection, or explicit public-authority configuration. Copying the local process to a server would either omit the Weave executable or expose credentials through ad hoc shell state.

## Decision

`Dockerfile.workbench` builds the Workbench client profile and its native Landlock launcher under the repository's pinned Node and pnpm versions, copies the trusted Weave executable from a selected Weave image, and starts the supported `dsh --profile workbench` entry. The native launcher is built in the image build stage and lets Linux containers enforce workspace-write without privileged container capabilities. `deploy/workbench.compose.yml` persists the Harness home and workspace, joins the existing Weave Docker network, sends the business API key only to the Workbench Host, and requires the deployment to declare the browser authority accepted by DSH authentication. DSH continues to listen only on its container loopback address. A network-namespace-sharing Nginx gateway publishes a separate internal port and preserves DSH's launch-token authentication, WebSocket connections, streaming responses, and public Host header. The published port binds to host loopback unless an operator deliberately selects another address.

The DSH launch-token exchange remains the browser authentication mechanism. The authenticated URL is an operator-held secret; an unauthenticated request receives no application content. Weave server encryption keys never enter the Workbench container.

The [Docker context exclusions](../../../../.dockerignore) omit `.env*` files at every directory depth. The image copies the build workspace into its runtime stage, so local environment files must be excluded before Docker receives the context; deployment credentials enter only through runtime configuration.

The [image build](../../../../Dockerfile.workbench) keeps APT downloads and indexes, npm tool downloads, and the pnpm store in BuildKit caches. Interrupted downloads can resume without copying package caches into the runtime image; the frozen dependency lockfile and source build remain required.

## Alternatives considered

**Run the repository directly under systemd.** This would couple deployment to the server's Node version, package-manager installation, native dependency toolchain, and mutable checkout.

**Expose the Workbench port without DSH authentication.** Network reachability is not user authorization, and the Workbench Host can start model and tool execution, so a public unauthenticated listener is unacceptable.

**Make DSH itself listen on every interface.** DSH intentionally rejects this configuration because its Host can execute code. Keeping the Host on container loopback preserves that safety invariant while the gateway provides an explicit deployment boundary.

**Pass the complete Weave environment file into Workbench.** This would leak server-only encryption and administration secrets into a process that needs only a business API key and model-provider credentials.

## Consequences

The image is larger than a static frontend because it contains the DSH Host runtime, its built package graph, the workspace sandbox, and the Weave MCP executable. Low-memory servers should build the image elsewhere or provision temporary swap; the running Workbench does not need the build toolchain's peak memory. The gateway is an additional small container, but it does not implement a second identity system. Building the Landlock launcher adds a Linux-only native build step and requires a kernel that actually enforces Landlock; the launcher probe fails closed otherwise. In return, the deployed browser uses the same Workbench profile as local development, survives container replacement through mounted state, can honor the default workspace-write policy without privileged container capabilities or unconfined execution, and keeps Weave credentials on the Host side. Operators must retain the generated authenticated URL or an existing signed browser cookie and must set the exact public authority before startup.

## Cloud business-flow verification

The deployed Workbench completed a browser-originated supplier-risk review with the four-member `Rrb L3 Supplier Risk Team 001`. The run continued while the Workbench container was replaced, restored from the persisted DSH session after the `workspace` workspace was selected again, reached four of four stages, and exposed nine bound outputs. The independent reviewer reported a passing result after checking ten source records, eight effective findings, and two reassessment chains.

The pilot exposed four deployment and product defects. The first image lacked an enforceable workspace sandbox; the image now builds and probes Landlock instead of requesting privileged container capabilities. DSH's loopback-only safety rule prevented direct publication; the namespace-sharing gateway now preserves that rule and the launch-token exchange. Readiness details and the original routing prompt could leak backend English, identifiers, and raw lifecycle enums; readiness facts are now localized and the Workbench prompt requires human-facing states. Finally, a terminal workflow delivery summary was classified as an ordinary stage output, making a completed run claim that no final result existed. Workbench now distinguishes final files, a delivery summary, and stage outputs. A delivery summary is presented as a usable completion result while stating honestly that a runtime-local file bundle has not been synchronized to the cloud.

Large restored sessions can still take materially longer to render than a new session, and file paths produced inside a remote runtime are not portable browser downloads. These are explicit follow-up boundaries rather than reasons to weaken the sandbox or mislabel a summary as a synchronized file.
