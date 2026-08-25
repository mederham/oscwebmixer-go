package hub

import (
	"fmt"
	"math"
)

// generateColour produces a hex colour for the index-th of total items,
// evenly spaced around the hue wheel — a Go port of the JS version's
// generateColour (lib/utils/utils.js), which used hsl(hue, 50%, 40%).
func generateColour(total, index int) string {
	if total <= 0 {
		total = 1
	}
	hue := 360 * float64(index) / float64(total)
	r, g, b := hslToRGB(hue, 0.5, 0.4)
	return fmt.Sprintf("#%02x%02x%02x", r, g, b)
}

func hslToRGB(h, s, l float64) (r, g, b uint8) {
	c := (1 - math.Abs(2*l-1)) * s
	x := c * (1 - math.Abs(math.Mod(h/60, 2)-1))
	m := l - c/2

	var rp, gp, bp float64
	switch {
	case h < 60:
		rp, gp, bp = c, x, 0
	case h < 120:
		rp, gp, bp = x, c, 0
	case h < 180:
		rp, gp, bp = 0, c, x
	case h < 240:
		rp, gp, bp = 0, x, c
	case h < 300:
		rp, gp, bp = x, 0, c
	default:
		rp, gp, bp = c, 0, x
	}

	r = uint8((rp + m) * 255)
	g = uint8((gp + m) * 255)
	b = uint8((bp + m) * 255)
	return
}
