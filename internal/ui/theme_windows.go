package ui

import (
	"image"
	"image/color"
	"os"
	"path/filepath"
	"unsafe"

	"gioui.org/font"
	"gioui.org/font/opentype"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/liljestk/VIbeClean/internal/model"
)

// palette holds the workspace colors. The light and dark values match the
// HTML concept; high contrast uses the Windows system colors instead.
type palette struct {
	dark, contrast bool
	surface        color.NRGBA // workspace background
	chrome         color.NRGBA // sidebar, cards, table header and alternate rows
	well           color.NRGBA // hover and pressed fills
	ink            color.NRGBA // primary text
	quiet          color.NRGBA // secondary text
	line           color.NRGBA // control outlines
	blue           color.NRGBA // selected navigation and neutral bars
	accent         color.NRGBA // primary action and focus
	onAccent       color.NRGBA // text on accent
	selected       color.NRGBA // selected rows and navigation
	grow           color.NRGBA // growth
	free           color.NRGBA // freed space
	caution        color.NRGBA // items to check before removing
	track          color.NRGBA // meter track
}

func rgb(v uint32) color.NRGBA {
	return color.NRGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 255}
}

var (
	lightPalette = palette{
		surface: rgb(0xfcfdff), chrome: rgb(0xf2f5fa), well: rgb(0xe9eff7), ink: rgb(0x18263b),
		quiet: rgb(0x52627a), line: rgb(0xd8e0ec), blue: rgb(0x2657c4), accent: rgb(0x2657c4),
		onAccent: rgb(0xffffff), selected: rgb(0xe5edfc), grow: rgb(0x9b412b), free: rgb(0x136f73),
		caution: rgb(0x8a5a00), track: rgb(0xdce4f0),
	}
	darkPalette = palette{
		dark:    true,
		surface: rgb(0x111b25), chrome: rgb(0x192734), well: rgb(0x203442), ink: rgb(0xedf4f9),
		quiet: rgb(0xb0c2d1), line: rgb(0x3b5061), blue: rgb(0x91ceff), accent: rgb(0x84caff),
		onAccent: rgb(0x112438), selected: rgb(0x28475e), grow: rgb(0xf7aa91), free: rgb(0x81d7d1),
		caution: rgb(0xf0c674), track: rgb(0x3a4960),
	}
)

// systemPalette follows the Windows app theme and high contrast setting.
func systemPalette() palette {
	if highContrast() {
		return contrastPalette()
	}
	if prefersDark() {
		return darkPalette
	}
	return lightPalette
}

func contrastPalette() palette {
	sys := func(index int) color.NRGBA {
		value, _, _ := getSysColor.Call(uintptr(index))
		return color.NRGBA{R: uint8(value), G: uint8(value >> 8), B: uint8(value >> 16), A: 255}
	}
	const window, windowText, highlight, highlightText, grayText, hotLight = 5, 8, 13, 14, 17, 26
	background, foreground := sys(window), sys(windowText)
	return palette{
		contrast: true, surface: background, chrome: background, well: background,
		ink: foreground, quiet: foreground, line: foreground, blue: sys(hotLight),
		accent: sys(highlight), onAccent: sys(highlightText), selected: sys(highlight),
		grow: foreground, free: foreground, caution: foreground, track: sys(grayText),
	}
}

var (
	user32       = windows.NewLazySystemDLL("user32.dll")
	getSysColor  = user32.NewProc("GetSysColor")
	sysParams    = user32.NewProc("SystemParametersInfoW")
	dwmAttribute = windows.NewLazySystemDLL("dwmapi.dll").NewProc("DwmSetWindowAttribute")
)

func prefersDark() bool {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer key.Close()
	light, _, err := key.GetIntegerValue("AppsUseLightTheme")
	return err == nil && light == 0
}

func highContrast() bool {
	const getHighContrast, highContrastOn = 0x0042, 0x1
	var info struct {
		size  uint32
		flags uint32
		theme *uint16
	}
	info.size = uint32(unsafe.Sizeof(info))
	ok, _, _ := sysParams.Call(getHighContrast, uintptr(info.size), uintptr(unsafe.Pointer(&info)), 0)
	return ok != 0 && info.flags&highContrastOn != 0
}

// tintFrame colors the native title bar to match the sidebar. Windows
// versions without these attributes keep their default frame.
func tintFrame(hwnd uintptr, p palette) {
	if hwnd == 0 {
		return
	}
	const immersiveDarkMode, borderColor, captionColor, textColor = 20, 34, 35, 36
	const colorNone, colorDefault = 0xfffffffe, 0xffffffff
	ref := func(c color.NRGBA) uint32 { return uint32(c.R) | uint32(c.G)<<8 | uint32(c.B)<<16 }
	border, caption, title := uint32(colorNone), ref(p.chrome), ref(p.ink)
	if p.contrast {
		border, caption, title = colorDefault, colorDefault, colorDefault
	}
	var dark uint32
	if p.dark {
		dark = 1
	}
	for _, item := range []struct {
		attribute uintptr
		value     uint32
	}{{immersiveDarkMode, dark}, {borderColor, border}, {captionColor, caption}, {textColor, title}} {
		value := item.value
		_, _, _ = dwmAttribute.Call(hwnd, item.attribute, uintptr(unsafe.Pointer(&value)), unsafe.Sizeof(value))
	}
}

// sans is the interface typeface. Segoe UI ships with every supported
// Windows version; the shaper falls back to other system fonts otherwise.
const sans font.Typeface = "Segoe UI"

func newShaper() *text.Shaper {
	dir := filepath.Join(os.Getenv("WINDIR"), "Fonts")
	var faces []font.FontFace
	for _, file := range []struct {
		name     string
		typeface font.Typeface
		weight   font.Weight
	}{
		{"segoeui.ttf", sans, font.Normal},
		{"seguisb.ttf", sans, font.SemiBold},
		{"segoeuib.ttf", sans, font.Bold},
		{"seguisym.ttf", "Segoe UI Symbol", font.Normal},
	} {
		data, err := os.ReadFile(filepath.Join(dir, file.name))
		if err != nil {
			continue
		}
		face, err := opentype.Parse(data)
		if err != nil {
			continue
		}
		faces = append(faces, font.FontFace{Font: font.Font{Typeface: file.typeface, Weight: file.weight}, Face: face})
	}
	if len(faces) == 0 {
		return text.NewShaper()
	}
	return text.NewShaper(text.NoSystemFonts(), text.WithCollection(faces))
}

// theme is the palette plus the text shaper, shared by every widget.
type theme struct {
	palette
	shaper *text.Shaper
	access *accessFrame
}

// style describes one run of text.
type style struct {
	size   unit.Sp
	weight font.Weight
	color  color.NRGBA
	// lines limits wrapping; 0 means a single line and -1 means no limit.
	lines int
	align text.Alignment
	wrap  text.WrapPolicy
}

func (t *theme) body(c color.NRGBA) style  { return style{size: 14, color: c} }
func (t *theme) small(c color.NRGBA) style { return style{size: 12, color: c} }
func (t *theme) strong(size unit.Sp, c color.NRGBA) style {
	return style{size: size, weight: font.SemiBold, color: c}
}

func (s style) wrapped(lines int) style { s.lines = lines; return s }
func (s style) aligned(a text.Alignment) style {
	s.align = a
	return s
}

// label draws text. Text is truncated with an ellipsis only when the
// constraints leave too little room for it.
func (t *theme) label(gtx layout.Context, s style, txt string) layout.Dimensions {
	lines := s.lines
	switch {
	case lines == 0:
		lines = 1
	case lines < 0:
		lines = 0
	}
	record := op.Record(gtx.Ops)
	paint.ColorOp{Color: s.color}.Add(gtx.Ops)
	material := record.Stop()
	return widget.Label{MaxLines: lines, Alignment: s.align, WrapPolicy: s.wrap}.
		Layout(gtx, t.shaper, font.Font{Typeface: sans, Weight: s.weight}, s.size, txt, material)
}

// measure returns the natural single-line width of txt in pixels.
func (t *theme) measure(gtx layout.Context, s style, txt string) int {
	record := op.Record(gtx.Ops)
	gtx.Constraints = layout.Constraints{Max: image.Pt(1<<20, 1<<20)}
	s.lines = 1
	dims := t.label(gtx, s, txt)
	record.Stop()
	return dims.Size.X
}

// tone returns the color for a signed change.
func (p palette) tone(delta int64) color.NRGBA {
	switch {
	case delta > 0:
		return p.grow
	case delta < 0:
		return p.free
	}
	return p.ink
}

// removal returns the mark color of a removal level: freed-space teal for
// safe wins, amber for items to check, blue for software to uninstall and
// quiet gray for everything to keep.
func (p palette) removal(r model.Removal) color.NRGBA {
	switch r {
	case model.SafeToClear:
		return p.free
	case model.CheckFirst:
		return p.caution
	case model.Uninstall:
		return p.blue
	}
	return p.quiet
}

func withAlpha(c color.NRGBA, alpha uint8) color.NRGBA {
	c.A = alpha
	return c
}
