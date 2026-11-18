package testsrc

import (
	"strings"
	"testing"
)

func TestEscapeFilterPath(t *testing.T) {
	cases := map[string]string{
		`C:\Windows\Fonts\consola.ttf`: `C\:/Windows/Fonts/consola.ttf`,
		`C:/Windows/Fonts/consola.ttf`: `C\:/Windows/Fonts/consola.ttf`,
		`/usr/share/fonts/a.ttf`:       `/usr/share/fonts/a.ttf`,
		`rel\overlay.txt`:              `rel/overlay.txt`,
	}
	for in, want := range cases {
		if got := EscapeFilterPath(in); got != want {
			t.Errorf("EscapeFilterPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidate(t *testing.T) {
	ok := Config{Width: 640, Height: 360, FPS: 30, TextFile: "o.txt"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*Config){
		"odd width":   func(c *Config) { c.Width = 641 },
		"zero fps":    func(c *Config) { c.FPS = 0 },
		"no textfile": func(c *Config) { c.TextFile = "" },
		"quote":       func(c *Config) { c.FontFile = "a'b.ttf" },
	} {
		c := ok
		mod(&c)
		if c.Validate() == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestBuildArgs(t *testing.T) {
	args := BuildArgs(Config{Width: 1280, Height: 720, FPS: 60, FontFile: `C:\Windows\Fonts\consola.ttf`},
		`C:\tmp\ov.txt`)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-f lavfi -i testsrc2=size=1280x720:rate=60",
		"-f rawvideo -pix_fmt nv12 pipe:1",
		`fontfile='C\:/Windows/Fonts/consola.ttf'`,
		`text='frame %{n}  pts %{pts\:hms}'`,
		`textfile='C\:/tmp/ov.txt':reload=1`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q:\n%s", want, joined)
		}
	}
	if strings.Count(joined, "drawtext=") != 2 {
		t.Errorf("want exactly two drawtext filters: %s", joined)
	}
	// No font configured: no fontfile option at all.
	if strings.Contains(strings.Join(BuildArgs(Config{Width: 64, Height: 64, FPS: 30}, "o.txt"), " "), "fontfile") {
		t.Error("fontfile should be omitted when unset")
	}
}
