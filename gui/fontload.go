package gui

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gioui.org/font"
	"gioui.org/font/gofont"
	"gioui.org/font/opentype"
)

// FontSet is the typeface configuration the theme is built from.
//
// Gio v0.10 already consults the operating system's fonts through go-text's
// fontscan, which handles CJK fallback on a well-configured desktop. We do not
// rely on that alone: minimal Linux images (containers, netboot, some NAS
// distros) ship a broken or empty font index, and the failure mode there is a
// window full of tofu boxes with no explanation. So we additionally locate a
// CJK font file ourselves and load it explicitly.
type FontSet struct {
	Collection []font.FontFace
	UI         font.Typeface
	Mono       font.Typeface

	// HasCJK reports whether Chinese text can be rendered. It drives the
	// default UI language: showing Chinese labels we cannot draw is worse than
	// showing English ones.
	HasCJK bool

	// CJKPath is the font file backing HasCJK, for display in the about panel.
	CJKPath string
}

// LoadFonts builds the initial font set. It is deliberately cheap — only a
// handful of os.Stat calls — so the window can open immediately. The actual
// CJK font file is parsed later by [LoadCJKFaces] while the splash screen is
// up.
func LoadFonts() *FontSet {
	fs := &FontSet{
		Collection: gofont.Collection(),
		UI:         "Go",
		Mono:       "Go Mono",
	}
	if path, ok := FindCJKFont(); ok {
		fs.HasCJK = true
		fs.CJKPath = path
	}
	return fs
}

// cjkCandidates returns absolute font paths to try, best first. Smaller
// single-script files come before the big pan-CJK collections: parsing a 20 MB
// .ttc costs a few hundred milliseconds and five faces we will never use.
func cjkCandidates() []string {
	switch runtime.GOOS {
	case "windows":
		dirs := []string{}
		if w := os.Getenv("WINDIR"); w != "" {
			dirs = append(dirs, filepath.Join(w, "Fonts"))
		}
		if l := os.Getenv("LOCALAPPDATA"); l != "" {
			dirs = append(dirs, filepath.Join(l, "Microsoft", "Windows", "Fonts"))
		}
		names := []string{
			"msyh.ttc", "msyh.ttf", // 微软雅黑
			"msyhl.ttc", "Deng.ttf", // 等线
			"simhei.ttf", // 黑体
			"simsun.ttc", "simsun.ttf",
			"msjh.ttc", // 微軟正黑體
		}
		var out []string
		for _, d := range dirs {
			for _, n := range names {
				out = append(out, filepath.Join(d, n))
			}
		}
		return out

	case "darwin":
		return []string{
			"/System/Library/Fonts/PingFang.ttc",
			"/System/Library/Fonts/Hiragino Sans GB.ttc",
			"/System/Library/Fonts/STHeiti Light.ttc",
			"/System/Library/Fonts/STHeiti Medium.ttc",
			"/Library/Fonts/Arial Unicode.ttf",
			"/System/Library/Fonts/Supplemental/Songti.ttc",
		}

	default: // linux, bsd
		return []string{
			// Debian/Ubuntu single-script Noto, the cheapest good option.
			"/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
			"/usr/share/fonts/opentype/noto/NotoSansCJKsc-Regular.otf",
			"/usr/share/fonts/truetype/noto/NotoSansCJKsc-Regular.otf",
			// Fedora/Arch layouts.
			"/usr/share/fonts/noto-cjk/NotoSansCJK-Regular.ttc",
			"/usr/share/fonts/adobe-source-han-sans/SourceHanSansSC-Regular.otf",
			"/usr/share/fonts/opentype/source-han-sans/SourceHanSansSC-Regular.otf",
			// Lightweight fallbacks common on embedded/NAS systems.
			"/usr/share/fonts/truetype/wqy/wqy-microhei.ttc",
			"/usr/share/fonts/wenquanyi/wqy-microhei/wqy-microhei.ttc",
			"/usr/share/fonts/truetype/wqy/wqy-zenhei.ttc",
			"/usr/share/fonts/truetype/droid/DroidSansFallbackFull.ttf",
			"/usr/share/fonts/truetype/droid/DroidSansFallback.ttf",
		}
	}
}

// fontSearchDirs are walked when no candidate path matched.
func fontSearchDirs() []string {
	var dirs []string
	switch runtime.GOOS {
	case "windows":
		if w := os.Getenv("WINDIR"); w != "" {
			dirs = append(dirs, filepath.Join(w, "Fonts"))
		}
	case "darwin":
		dirs = append(dirs, "/System/Library/Fonts", "/Library/Fonts")
	default:
		dirs = append(dirs, "/usr/share/fonts", "/usr/local/share/fonts")
	}
	if home, err := os.UserHomeDir(); err == nil {
		switch runtime.GOOS {
		case "darwin":
			dirs = append(dirs, filepath.Join(home, "Library", "Fonts"))
		case "windows":
		default:
			dirs = append(dirs, filepath.Join(home, ".local", "share", "fonts"), filepath.Join(home, ".fonts"))
		}
	}
	return dirs
}

// cjkNameHints match filenames of fonts known to carry Han glyphs.
var cjkNameHints = []string{
	"notosanscjk", "notoserifcjk", "notosanssc", "notosanstc", "notosanshk",
	"sourcehansans", "sourcehanserif", "wqy-microhei", "wqy-zenhei",
	"droidsansfallback", "msyh", "simhei", "simsun", "pingfang", "hiragino",
	"stheiti", "unifont", "arphic", "uming", "ukai", "microhei", "zenhei",
	"opposans", "harmonyos_sans_sc", "arialuni",
}

// FindCJKFont locates a font file with Chinese coverage. The walk is bounded so
// a pathological font directory cannot stall startup.
func FindCJKFont() (string, bool) {
	for _, p := range cjkCandidates() {
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Size() > 0 {
			return p, true
		}
	}

	deadline := time.Now().Add(600 * time.Millisecond)
	seen := 0
	for _, dir := range fontSearchDirs() {
		var found string
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // unreadable subtree, keep going
			}
			if seen++; seen > 20000 || time.Now().After(deadline) {
				return filepath.SkipAll
			}
			if d.IsDir() {
				return nil
			}
			name := strings.ToLower(d.Name())
			switch {
			case strings.HasSuffix(name, ".ttf"),
				strings.HasSuffix(name, ".ttc"),
				strings.HasSuffix(name, ".otf"),
				strings.HasSuffix(name, ".otc"):
			default:
				return nil
			}
			for _, hint := range cjkNameHints {
				if strings.Contains(name, hint) {
					found = path
					return filepath.SkipAll
				}
			}
			return nil
		})
		if found != "" {
			return found, true
		}
	}
	return "", false
}

// maxFontBytes caps how large a font file we are willing to read. Pan-CJK
// collections run to ~40 MB; anything beyond that is not a font we want.
const maxFontBytes = 64 << 20

// LoadCJKFaces parses the font file at path and returns its faces, ready to be
// appended to a collection. It is slow enough (tens to hundreds of
// milliseconds) that callers should run it off the UI goroutine — which is
// exactly what the splash screen exists for.
func LoadCJKFaces(path string, logger *slog.Logger) ([]font.FontFace, error) {
	if logger == nil {
		logger = slog.Default()
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Size() > maxFontBytes {
		logger.Warn("cjk font too large, skipping", "path", path, "bytes", st.Size())
		return nil, nil
	}
	start := time.Now()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	faces, err := opentype.ParseCollection(data)
	if err != nil {
		return nil, err
	}
	if len(faces) == 0 {
		return nil, nil
	}
	// A pan-CJK .ttc carries SC/TC/HK/JP/KR cuts of the same design. Keeping
	// one avoids paying for five near-identical fallbacks on every glyph miss;
	// pickCJKFace decides which one.
	base := faces[pickCJKFace(faces)]
	out := cjkWeightVariants(base)
	logger.Debug("cjk font loaded",
		"path", path,
		"typeface", string(base.Font.Typeface),
		"faces", len(out),
		"bytes", st.Size(),
		"took", time.Since(start).Round(time.Millisecond),
	)
	return out, nil
}

// scWeights are the family-name markers of the Simplified Chinese cut, in
// preference order. Pan-CJK collections order their faces JP first, so taking
// faces[0] blindly renders Han characters with Japanese glyph variants — legible,
// but visibly wrong to a Chinese reader.
var scMarkers = []string{"sc", "simplified", "cn", "hans"}

// pickCJKFace returns the index of the face to use, preferring the Simplified
// Chinese cut and falling back to the first face.
func pickCJKFace(faces []font.FontFace) int {
	for _, marker := range scMarkers {
		for i, f := range faces {
			name := strings.ToLower(string(f.Font.Typeface))
			// Match on a word/suffix boundary so "sc" does not hit "Sans".
			for _, field := range strings.FieldsFunc(name, func(r rune) bool {
				return r == ' ' || r == '-' || r == '_'
			}) {
				if field == marker || strings.HasSuffix(field, marker) {
					return i
				}
			}
		}
	}
	return 0
}

// cjkWeightVariants registers one parsed face under every weight the UI asks
// for.
//
// This exists because of how Gio resolves fonts. The theme pins every label's
// Typeface to "Go" (see NewTheme), and Gio never tells go-text which script it
// is shaping, so our explicitly-loaded CJK font is only reachable through
// fontscan's user-provided tier — which prunes candidates by weight before
// checking coverage. A face registered only at Normal is therefore invisible to
// any label that sets Font.Weight, and every section title, card header and
// button does exactly that. The result was Chinese body text rendering fine
// while every heading turned into tofu boxes.
//
// The variants share the same underlying Face, so CJK headings are not visually
// bolder than body text. That is a deliberate trade: identical weight beats
// missing glyphs, and synthetic emboldening is not available here.
func cjkWeightVariants(base font.FontFace) []font.FontFace {
	weights := []font.Weight{font.Normal, font.Medium, font.SemiBold, font.Bold}
	out := make([]font.FontFace, 0, len(weights))
	for _, w := range weights {
		f := base.Font
		f.Weight = w
		f.Style = font.Regular
		out = append(out, font.FontFace{Font: f, Face: base.Face})
	}
	return out
}

// goCollection returns the built-in Go font faces. It exists so tests can
// build a theme without touching the host's font configuration.
func goCollection() []font.FontFace { return gofont.Collection() }
