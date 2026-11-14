"""Prepare the Geist fonts gotm embeds.

Windows GDI, which gotm draws with, cannot switch on OpenType features, and
Geist's default figures are proportional. A task manager needs every digit the
same width (columns of changing numbers must not shimmer; Meridian asks for
tabular figures everywhere), so this script makes Geist's own tabular figures
the default: each character whose glyph the font's `tnum` feature replaces is
mapped straight to the replacement glyph. Nothing is redrawn and no name
changes; the SIL Open Font License 1.1 allows this (Geist declares no Reserved
Font Name), and internal/ui/fonts/README.md records the change.

    pip install fonttools
    python tools/fonts/tabular.py <folder with Geist-Regular.ttf, Geist-SemiBold.ttf> internal/ui/fonts
"""
import os
import sys

from fontTools.ttLib import TTFont

FILES = ("Geist-Regular.ttf", "Geist-SemiBold.ttf")


def tabular(font):
    gsub = font["GSUB"].table
    swaps = {}
    for record in gsub.FeatureList.FeatureRecord:
        if record.FeatureTag != "tnum":
            continue
        for index in record.Feature.LookupListIndex:
            for sub in gsub.LookupList.Lookup[index].SubTable:
                if sub.LookupType == 7:  # extension
                    sub = sub.ExtSubTable
                if sub.LookupType == 1:  # single substitution
                    swaps.update(sub.mapping)
    if not swaps:
        raise SystemExit("no tnum single substitutions found")
    changed = 0
    for table in font["cmap"].tables:
        if not table.isUnicode():
            continue
        for code, glyph in list(table.cmap.items()):
            if glyph in swaps:
                table.cmap[code] = swaps[glyph]
                changed += 1
    return changed


def main(src, dst):
    for name in FILES:
        font = TTFont(os.path.join(src, name))
        changed = tabular(font)
        cmap, widths = font.getBestCmap(), font["hmtx"]
        digits = {widths[cmap[ord(c)]][0] for c in "0123456789"}
        if len(digits) != 1:
            raise SystemExit(f"{name}: digits are still {sorted(digits)} wide")
        font.save(os.path.join(dst, name))
        print(f"{name}: {changed} characters now tabular, digit width {digits.pop()}")


if __name__ == "__main__":
    main(sys.argv[1], sys.argv[2])
