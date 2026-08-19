// Package shapes is the fixture's geometry vocabulary: one interface with two
// implementers, one negative case, and a constant with a deliberate reference
// web spanning both fixture packages.
package shapes

// Shape is the fixture's primary interface. Exactly two types in this module
// implement it: Rect and Circle.
type Shape interface {
	// Area reports the shape's area.
	Area() float64
	// Name reports the shape's short label.
	Name() string
}

// Named is a second, narrower interface, so go_implementations has more than
// one answer to give in the type-to-interface direction.
type Named interface {
	Name() string
}

// Rect is an axis-aligned rectangle.
//
// Its doc comment runs to two paragraphs on purpose, so go_describe has a
// multi-line doc to return and clamp.
type Rect struct {
	// W is the width.
	W float64
	// H is the height.
	H float64
}

// Area implements Shape for Rect.
func (r Rect) Area() float64 { return r.W * r.H }

// Name implements Shape for Rect.
func (r Rect) Name() string { return "rect" }

// Circle is a circle of radius R.
type Circle struct {
	// R is the radius.
	R float64
}

// Area implements Shape for Circle.
func (c Circle) Area() float64 { return 3.14159 * c.R * c.R }

// Name implements Shape for Circle.
func (c Circle) Name() string { return "circle" }

// Unit is the fixture's reference magnet: it is used twice here and three times
// in package report.
const Unit = 1.0

// UnitRect is the one-by-one rectangle.
var UnitRect = Rect{W: Unit, H: Unit}

// Scale multiplies both dimensions of r by f.
func Scale(r Rect, f float64) Rect {
	return Rect{W: r.W * f, H: r.H * f}
}

// notShape has an Area but no Name, so it does NOT implement Shape. It is the
// negative case for go_implementations.
type notShape struct{}

// Area exists only so notShape is a near-miss rather than an obvious one.
func (notShape) Area() float64 { return 0 }
