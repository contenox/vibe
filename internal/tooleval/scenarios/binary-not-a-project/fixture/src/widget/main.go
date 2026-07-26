package widget

// New returns a new Widget.
func New() *Widget { return &Widget{} }

// Widget is a thing that renders.
type Widget struct{}

// Render draws the widget.
func (w *Widget) Render() string { return "widget" }
