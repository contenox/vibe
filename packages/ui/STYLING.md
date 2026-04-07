# Contenox UI Styling Guide

This document defines the visual identity for Contenox products. All UI work — in
Beam, the marketing site, and any future surface — must follow these rules.

---

## Brand Colors

| Role | Light | Dark | Token prefix |
|------|-------|------|--------------|
| **Primary (brand)** | Emerald `#10b981` | Emerald `#10b981` | `--color-primary-*` |
| **Accent** | Teal `#14b8a6` | Teal `#14b8a6` | `--color-accent-*` / `--color-dark-accent-*` |
| **Secondary** | Slate `#64748b` | Slate `#64748b` | `--color-secondary-*` |
| **Error** | Red `#dc2626` | Red `#b91c1c` | `--color-error-*` |
| **Success** | Green `#22c55e` | Green `#22c55e` | `--color-success-*` |
| **Warning** | Yellow `#eab308` | Yellow `#eab308` | `--color-warning-*` |
| **Info** | Sky `#0ea5e9` | Sky `#0ea5e9` | `--color-info-*` |

Each palette has 10 shades (50-900) plus a base alias. Use semantic token names,
not raw hex values.

### Rules

- **Never hardcode hex colors in components.** Always reference tokens via
  Tailwind classes (`bg-primary-500`) or CSS variables (`var(--color-primary-500)`).
- The `--color-brand` alias always points to `--color-primary-500`. Use it when
  you mean "the Contenox brand color" rather than "the primary UI color."
- Semantic colors (error, success, warning, info) are fixed across themes and
  must not be repurposed for decorative use.

---

## Typography

| Role | Font | Token |
|------|------|-------|
| **Display / headings** | Geist | `--font-display` |
| **Body text** | Geist | `--font-body` |
| **Code / monospace** | Geist Mono | `--font-mono` |

### Rules

- No other font families. Satoshi, Inter, and system-ui are only fallbacks.
- Use `font-display` for headings (h1-h3) and `font-body` for everything else.
- Use `font-mono` for code blocks, terminal output, and inline code.
- Do not import additional fonts in components.

---

## Dark Mode

Dark mode uses **warm-shifted neutral surfaces** — not cool blue/navy.

### Surface philosophy

All dark surfaces use oklch with:
- **Hue**: 60 (neutral warm zone between yellow and olive)
- **Chroma**: 0.002-0.005 (imperceptible warmth, just removes cold blue cast)
- **Lightness**: Stepped from 0.10 (darkest) to 0.50 (lightest surface)

### Rules

- Dark surfaces must never have a blue or navy tint.
- Components use `dark:` prefix with `dark-*` token counterparts
  (e.g., `bg-surface-100 dark:bg-dark-surface-100`).
- The `@dark` block in `index.css` maps `--color-*` to `--color-dark-*`
  automatically. Prefer using the non-prefixed tokens when the `@dark` mapping
  handles the switch (e.g., `bg-primary` works in both modes).
- Test every component in both modes. If a color looks blue-tinted in dark mode,
  it's wrong.

---

## Border Radius

| Token | Value | Use |
|-------|-------|-----|
| `--radius-sm` | 6px | Small elements (badges, chips) |
| `--radius-md` | 8px | Inputs, buttons |
| `--radius-lg` | 10px | Cards, panels (base) |
| `--radius-xl` | 14px | Modals, large containers |

Base: `--radius: 0.625rem` (10px). All others derive from it.

### Rules

- Prefer token-derived radii over arbitrary Tailwind values.
- Nested elements should use equal or smaller radius than their parent.

---

## Theming Contract

The design token system in `packages/ui/src/index.css` is the single source of
truth. Theming works because:

1. **`@theme` block** defines all light-mode tokens and all `dark-*` counterparts.
2. **`@dark` block** remaps `--color-*` to `--color-dark-*` when `.dark` is active.
3. **Components reference tokens**, not raw colors.

To create a new theme (hypothetical): duplicate the token values in `@theme`,
override in a new CSS scope. No component code changes needed.

### What goes in tokens

- All colors (brand, semantic, surface, text, background, gradient)
- Font families
- Radius values
- Spacing overrides (only custom ones beyond Tailwind defaults)
- Easing curves

### What stays in components

- Layout (flex, grid, positioning)
- Responsive breakpoints
- Variant-specific structural differences (padding, gaps)
- Animation keyframes

---

## Component Variants

### When to add a variant to an existing component

- The visual change is a color/style swap on the same structure.
- The component already has a `variant` or `palette` prop.
- Examples: a new Button color, a new Card border style.

### When to create a new component

- The structure (DOM, layout, slots) is meaningfully different.
- The new thing has its own props that don't map to existing variants.
- Examples: a TerminalOutput component vs a Card, a CommandPanel vs a Dialog.

### Rules

- Variants use the `cn()` utility with conditional Tailwind classes.
- Variant class strings must only reference design tokens.
- Add new variants to the component's TypeScript type union.
- Document new variants with a brief comment in the component file.

---

## Checklist for New UI Work

- [ ] Colors reference tokens, not hex literals
- [ ] Works in both light and dark mode
- [ ] Uses Geist / Geist Mono fonts only
- [ ] Border radius uses token-derived values
- [ ] No blue/navy tint in dark mode surfaces
- [ ] Variant props are typed and documented
