package main

import (
	"fmt"
	"strings"
)

// The screen as an SVG "screenshot": a dark terminal window with a title
// bar. Each run of same-styled text is placed at its exact column and
// stretched to its cell count (textLength), so columns line up whatever
// monospace font the viewer has.

const (
	cellW    = 8.4 // advance of a 14 px monospace font
	lineH    = 18.0
	fontSize = 14
	pad      = 14.0
	barH     = 30.0
)

// GitHub-dark-like palette.
var (
	bgColor = "#0d1117"
	fgColor = "#c9d1d9"
	palette = map[int]string{
		31: "#ff7b72", 32: "#3fb950", 33: "#d29922", 34: "#58a6ff",
		35: "#bc8cff", 36: "#39c5cf", 37: "#f0f6fc", 90: "#6e7681",
		41: "#ff7b72", 42: "#3fb950", 43: "#d29922", 44: "#1f6feb",
		45: "#bc8cff", 46: "#39c5cf", 47: "#f0f6fc",
	}
)

// colors resolves a cell's style to fill and background (bg "" = none).
func colors(st style) (fg, bg string) {
	fg, bg = fgColor, ""
	if c, ok := palette[st.fg]; ok {
		fg = c
	}
	if c, ok := palette[st.bg]; ok {
		bg = c
	}
	if st.rev {
		if bg == "" {
			bg = bgColor
		}
		fg, bg = bg, fg
	}
	return fg, bg
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// svg renders the grid inside a window titled title.
func svg(v *vt, title string) string {
	width := pad*2 + float64(v.w)*cellW
	height := barH + pad*2 + float64(v.h)*lineH
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f" role="img" aria-label="%s">`+"\n",
		width, height, width, height, xmlEscape(title))
	fmt.Fprintf(&b, `<rect width="100%%" height="100%%" rx="10" fill="%s"/>`+"\n", bgColor)
	fmt.Fprintf(&b, `<rect width="100%%" height="%.0f" rx="10" fill="#161b22"/><rect y="%.0f" width="100%%" height="10" fill="#161b22"/>`+"\n", barH, barH-10)
	for i, c := range []string{"#ff5f57", "#febc2e", "#28c840"} {
		fmt.Fprintf(&b, `<circle cx="%d" cy="%.0f" r="6" fill="%s"/>`, 20+i*20, barH/2, c)
	}
	fmt.Fprintf(&b, "\n"+`<text x="%.0f" y="%.0f" fill="#8b949e" font-family="-apple-system,Segoe UI,Helvetica,Arial,sans-serif" font-size="12" text-anchor="middle">%s</text>`+"\n",
		width/2, barH/2+4, xmlEscape(title))
	fmt.Fprintf(&b, `<g font-family="ui-monospace,SFMono-Regular,Menlo,Consolas,'Liberation Mono','DejaVu Sans Mono',monospace" font-size="%d" xml:space="preserve">`+"\n", fontSize)

	for r, row := range v.grid {
		y := barH + pad + float64(r)*lineH
		for c := 0; c < len(row); {
			st := row[c].st
			end := c
			for end < len(row) && row[end].st == st {
				end++
			}
			var text strings.Builder
			for _, cl := range row[c:end] {
				text.WriteRune(cl.r)
			}
			fg, bg := colors(st)
			x := pad + float64(c)*cellW
			n := float64(end - c)
			if bg != "" {
				fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.0f" fill="%s"/>`, x, y, n*cellW, lineH, bg)
			}
			attrs := ""
			if st.bold {
				attrs += ` font-weight="bold"`
			}
			if st.dim {
				attrs += ` opacity="0.6"`
			}
			// One <text> per word, at its own column: viewers collapse
			// spaces, so spaces must never position anything.
			runes := []rune(text.String())
			for i := 0; i < len(runes); {
				if runes[i] == ' ' {
					i++
					continue
				}
				j := i
				for j < len(runes) && runes[j] != ' ' {
					j++
				}
				fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" fill="%s" textLength="%.1f" lengthAdjust="spacingAndGlyphs"%s>%s</text>`,
					x+float64(i)*cellW, y+lineH*0.75, fg, float64(j-i)*cellW, attrs, xmlEscape(string(runes[i:j])))
				i = j
			}
			c = end
		}
		b.WriteString("\n")
	}
	b.WriteString("</g>\n</svg>\n")
	return b.String()
}
