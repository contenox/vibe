// Package report consumes shapes. It is the far end of the fixture's
// cross-package reference web.
package report

import (
	"fmt"
	"strings"

	"example.com/fixture/shapes"
)

// Unit is report's own label constant. It deliberately shares a name with
// shapes.Unit so that the bare name "Unit" is ambiguous across the module.
const Unit = "u"

// Default is a Rect built from the shapes.Unit constant.
var Default = shapes.Rect{W: shapes.Unit, H: shapes.Unit}

// Total sums the areas of every shape.
func Total(list []shapes.Shape) float64 {
	sum := 0.0
	for _, s := range list {
		sum += s.Area()
	}
	return sum
}

// Describe renders one shape as "name=area".
func Describe(s shapes.Shape) string {
	return fmt.Sprintf("%s=%.2f%s", s.Name(), s.Area(), Unit)
}

// Lines renders every shape, one per line.
func Lines(list []shapes.Shape) string {
	var b strings.Builder
	for _, s := range list {
		b.WriteString(Describe(s))
		b.WriteString("\n")
	}
	return b.String()
}

// Doubled scales Default by two units.
func Doubled() shapes.Rect {
	return shapes.Scale(Default, 2*shapes.Unit)
}
