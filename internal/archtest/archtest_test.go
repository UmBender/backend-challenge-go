package archtest

import (
	"strings"
	"testing"
)

// The fixture proves the checker fires; otherwise the real test below would
// pass vacuously.
func TestFloatCheckerDetectsEveryKnownTrap(t *testing.T) {
	findings, err := FloatFindings("testdata/floaty")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range findings {
		got[f.Rule] = true
		if strings.Contains(f.Pos, "clean.go") {
			t.Errorf("false positive in clean.go: %+v", f)
		}
	}
	for _, rule := range []string{
		"float64",
		"float32",
		"strconv.ParseFloat",
		"strconv.FormatFloat",
		`import "math"`,
		`import "math/big"`,
		".Float64()",
		"json.Number",
		"float format verb",
	} {
		if !got[rule] {
			t.Errorf("rule %q not reported", rule)
		}
	}
}

// moneyPaths grows as adapters that parse, persist or serialize money are added.
var moneyPaths = []string{
	"../domain",
}

// Guarantee 1 of §5: no float32/float64 on money paths.
func TestNoFloatingPointOnMoneyPaths(t *testing.T) {
	for _, dir := range moneyPaths {
		findings, err := FloatFindings(dir)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		for _, f := range findings {
			t.Errorf("%s: %s", f.Pos, f.Rule)
		}
	}
}
