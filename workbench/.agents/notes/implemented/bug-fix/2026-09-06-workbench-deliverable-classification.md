# Agent Note: Workbench deliverable classification

Status: implemented

English | [中文](2026-09-06-workbench-deliverable-classification.zh.md)

## Problem

A member's intermediate file can retain a final label in the WorkTask cache after Weave corrects its classification, because the number of files has not changed. This presents unfinished team work as available final delivery.

## Decision

The Host treats published-workflow files from non-delivery nodes as stage outputs. Exact-run activity references can demote a cached file to a stage output without fetching its content again. File content and identity remain intact. The delivery node retains final-file and summary classification.

## Alternatives considered

**Refresh only on file count changes.** Classification can change independently of count, so this leaves incorrect labels visible.

**Refetch every file on each poll.** The activity response already supplies the authoritative stage classification; repeating large content reads adds no evidence.

## Consequences

The current view follows Weave's classification while retained historical snapshots remain unchanged. Missing activity references do not erase known files or invent final delivery. The regression records a final file, applies an activity observation with the same file count, and verifies the stage label and unchanged content.
