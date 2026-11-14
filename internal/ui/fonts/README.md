# Bundled fonts

gotm follows the Meridian design system, which sets everything in Geist.
Geist is not a Windows font, so gotm embeds two weights and registers them for
its own process only at start-up (nothing is installed on the PC).

| File | Source | Licence |
|---|---|---|
| `Geist-Regular.ttf` | [vercel/geist-font](https://github.com/vercel/geist-font) v1.9.2, `Geist/ttf/` | SIL Open Font License 1.1 (`OFL.txt`) |
| `Geist-SemiBold.ttf` | same | same |

**Modified from the release.** Windows GDI cannot enable OpenType features and
Geist's default figures are proportional, so `tools/fonts/tabular.py` maps the
ten digits to the tabular figures the font already contains (its `tnum`
feature). No glyph is redrawn and no font name is changed. To update: download
a newer release, run the script, and rebuild.

`OFL.txt` is copied into THIRD-PARTY-NOTICES.txt by `build.ps1`.
