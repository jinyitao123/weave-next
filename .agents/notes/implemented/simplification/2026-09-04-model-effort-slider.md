# Agent Note: Compact model effort slider

Status: implemented

English | [中文](2026-09-04-model-effort-slider.zh.md)

## Problem

The composer exposed reasoning effort through a second drill-in list of radio rows. A user had to open the model control, choose the effort row, interpret six text-only choices, and then reopen the control after every successful selection because the popover closed. The interaction technically worked but did not make the current level, the relative scale, or quick adjustment feel like one coherent model setting.

## Decision

The composer model trigger now opens a compact overview. One summary row shows the current model and localized standard effort name and drills into the provider-grouped model list. Both panes keep one card width, and a successful model choice returns to the overview instead of closing the card. When the exact model advertises reasoning metadata, the overview renders those efforts as a discrete native range input over a filled rail with one mark per advertised level. Pointer movement updates a local draft immediately and persists the complete session selection on release, without dimming or closing the popover; a rejected change keeps the existing toast path and restores the settled value.

The slider derives its levels and default only from Host-reported model metadata. It does not invent unsupported effort values, and a model without reasoning metadata shows no slider. The native range input owns arrow keys and accessible slider semantics; the surrounding popover is a dialog, while only the drilled model list uses menu roles. Standard effort ids receive localized display names, and unknown adapter-owned names pass through unchanged.

## Alternatives considered

**Restyle the radio list.** Rejected: better spacing would not remove the extra navigation step or show that effort values form an ordered scale.

**Use a continuous numeric slider.** Rejected: providers accept named discrete levels, not an arbitrary number, and interpolating would present values the selected model cannot serve.

**Put a permanent slider on the composer toolbar.** Rejected: it would consume primary input space for an infrequent adjustment and imply that every model supports reasoning levels.

**Close the popover after every slider movement.** Rejected: it makes comparison expensive and turns a direct-manipulation control back into repeated menu navigation.

## Consequences

The current model and effort are visible together, one click exposes the scale, and adjustment remains inside one stable card. The control stays keyboard accessible, honors reduced-motion preferences, and supports light and dark theme tokens. Provider metadata still determines the complete selectable vocabulary and request value, so the visual refinement changes no model-routing contract.
