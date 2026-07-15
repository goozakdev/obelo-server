# Juice Box design language — tvOS (10-foot) adaptation

The web app's visual language, restated for a focus-driven TV UI. The spirit is fixed; the chrome follows tvOS conventions. Goal: the TV app and web app unmistakably feel like one product.

## The three rules (unchanged from the web app)

1. **Flat wireframe.** A near-black canvas; separation comes from hairline borders and spacing, never from card fills, gradients, or drop shadows. Surfaces are transparent-to-barely-tinted.
2. **Artwork carries the color.** UI chrome is monochrome + one accent; posters and backgrounds are the only saturated things on screen. Media-first: the biggest thing on any screen is artwork, not text or chrome.
3. **Bare lists.** Rows and grids without boxes around every item — dividers and whitespace do the work.

## Tokens

Same palette as the web app (`web/src/index.css`), dark-only — TV UIs are dark-canvas by nature, so there is no light mode to design:

| Token | Value | Use |
| --- | --- | --- |
| `bg` | `#0c0d10` | Canvas |
| `surface` | `#12141a` | Raised panels (menus, dialogs) |
| `surface2` | `#1a1d25` | Second-level raise |
| `border` | `#23272f` | Hairlines, dividers |
| `borderStrong` | `#333947` | Emphasized outlines |
| `text` | `#e9ecf2` | Primary text |
| `textDim` | `#8f97a6` | Secondary text, metadata lines |
| `accent` | `#b8e04d` | **The** brand mark: focus, progress, selection, primary buttons |
| `accentInk` | `#161f08` | Text/icons on accent fills |
| `ok` / `warn` / `error` | `#3ecf8e` / `#e6a23c` / `#f47174` | Status only, never decoration |

Corner radius stays small and unshowy: 6pt default, 4pt small (scale up only for full-bleed poster cards where tvOS focus styling needs it). Type: the system font (SF Pro on tvOS), regular weights; the web app's restraint (no display faces, weight ≤ 650) carries over. Respect tvOS HIG minimums — body ≥ 29pt equivalent; `textDim` for every secondary line rather than smaller sizes.

## Focus replaces hover

The web app's hover states translate to the tvOS focus engine, not to new invention:

- **Focused poster/card**: the system scale+parallax effect, plus a **2pt `accent` ring** — the lime ring *is* the Juice Box focus signature; don't use white glows or shadows. Hairline `border` on unfocused items is optional at 10 feet (it won't read from the couch); the accent ring on focus is mandatory.
- **Focused text row** (settings, track lists): `surface2` pill behind the row + text brightens `textDim → text`. No underlines.
- **Buttons**: default = transparent with `borderStrong` outline; primary/confirm = `accent` fill with `accentInk` label. One primary per screen. **Focused**: the default button takes the same 2pt `accent` ring as a card. The primary can't — a lime ring on a lime fill is invisible at ten feet — so it takes the system scale lift instead. Focus must change *something* on every button; this bullet once specified only the resting state, and the tvOS client faithfully implemented the hole, shipping 17 buttons that rendered identically focused and unfocused.

## Navigation chrome

The web app uses side tabs; on tvOS use the **standard top tab bar** (platform convention wins for the 10-foot swipe-down gesture): Home · per-Library tabs · Search · Settings. Everything below the bar is content — no persistent sidebars, no breadcrumbs.

## Screen patterns

- **Home**: full-width shelf rows (`Continue Watching`, `Up Next`, `Recently Added`) of poster cards, a `textDim` row label above each, no row boxes. Continue Watching cards carry a **2pt accent progress bar** flush along the card's bottom edge (from `resumePositionMs / durationMs`).
- **Library grid**: edge-to-edge poster grid, no card chrome; title/year label under each poster only on focus (media-first — let the wall of artwork breathe).
- **Detail**: full-bleed `background` artwork with a `bg` gradient scrim rising from the bottom third; logo artwork (when present) instead of a text title; metadata line in `textDim`; a single accent Play button (label from `resumePoint.mode`: "Play" vs "Continue"). Below the fold: seasons/episodes or editions as bare rows.
- **Player overlay**: fully custom (libmpv renders the video; there is no system transport to inherit) — which makes this the screen where the design language matters most. Keep it minimal: a single bottom scrub bar (progress in `accent`, buffered range `borderStrong`, no thumb until focused), title + metadata line above it in `text`/`textDim`, everything on a bottom-third `bg` gradient scrim, auto-hidden after ~4s of no remote activity. Track pickers (audio/subtitle/video) as plain focusable list overlays; source-tag subtitle entries subtly (`Fetched`/`Sidecar` in `textDim`) and mark styled originals no differently — don't invent icons.
- **Empty/error states**: a `textDim` sentence, at most one outlined action. Errors use `error` for the status line only — no red panels. `warn` amber is reserved for standing cautions, mirroring the web app's Degraded-backend usage.

## Overscan & layout

Keep all focusable content inside the tvOS safe area (60pt top/bottom, 90pt sides); only full-bleed artwork and scrims may run to the edges. Shelf rows scroll horizontally behind the safe-area edge — a peeking half-poster is the scroll affordance; never draw scroll bars.

## What *not* to bring

No card fills, no shadows, no glassmorphism/blur panels (except the system player overlay's own material), no Plex-amber, no rating-badge color coding, no more than one accent. When in doubt: remove the box, dim the text, let the poster do it.
