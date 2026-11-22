# Azure component kit

These React components are adapted from `slev12397/beautiful-ui`, under the MIT
license in `LICENSE`. Upstream source:
https://github.com/slev12397/beautiful-ui/tree/main/components

- `atoms/Button.tsx`: `components/atoms/Button.tsx`
- `atoms/Chip.tsx`: `components/atoms/Chip.tsx`
- `atoms/SegmentedControl.tsx`: `components/atoms/SegmentedControl.tsx`
- `atoms/StatusPill.tsx`: `components/atoms/StatusPill.tsx`
- `primitives/LoadingState.tsx`: `components/primitives/LoadingState.tsx`

They are copy-paste source components, not a runtime connection to the upstream
website and not a second global component provider. Their CSS tokens are scoped
to `html[data-theme="azure"]`; the classic theme never renders this kit.

Changes for AMA:

- Use a tiny local `cn` helper instead of pulling in class-variance-authority.
- Use AMA's scoped color/shadow tokens and existing test ids and callbacks.
- Keep only the non-video loading variants; no external media is loaded.
- Preserve keyboard behavior and accessible labels in the segmented control.

Keep the upstream copyright and license notice with these files.
