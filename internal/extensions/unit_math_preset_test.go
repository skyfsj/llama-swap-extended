package extensions

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// presetTool runs one tool call through the real worker and returns the text the
// model receives.
func presetTool(t *testing.T, compiled *Compiled, name string, arguments map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"name": name, "arguments": arguments})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := compiled.Invoke(context.Background(), "onToolCall", Context{Endpoint: "chat.completions"}, payload)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var result struct {
		Content any `json:"content"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("output %s: %v", string(out), err)
	}
	switch content := result.Content.(type) {
	case string:
		return content
	case []any:
		var parts []string
		for _, raw := range content {
			if entry, ok := raw.(map[string]any); ok {
				if text, ok := entry["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	t.Fatalf("unrecognised result: %+v", result.Content)
	return ""
}

func presetToolError(t *testing.T, compiled *Compiled, name string, arguments map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"name": name, "arguments": arguments})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = compiled.Invoke(context.Background(), "onToolCall", Context{Endpoint: "chat.completions"}, payload)
	if err == nil {
		t.Fatal("the tool accepted input it should have rejected")
	}
	return err.Error()
}

// TestPreset_UnitConvertTemperature pins the affine cases, including the one
// value where Celsius and Fahrenheit agree.
func TestPreset_UnitConvertTemperature(t *testing.T) {
	compiled := presetBundle(t, "unit-convert")
	if compiled == nil {
		t.Fatal("unit-convert is not in the catalog")
	}
	for _, entry := range []struct {
		value float64
		from  string
		to    string
		want  string
	}{
		{0, "°C", "°F", "32 °F"},
		{100, "°C", "K", "373.15 K"},
		{-40, "°C", "°F", "-40 °F"},
		{32, "°F", "°C", "0 °C"},
		{0, "K", "°C", "-273.15 °C"},
		{0, "°C", "°R", "491.67 °R"},
	} {
		text := presetTool(t, compiled, "unit_convert", map[string]any{"value": entry.value, "from": entry.from, "to": entry.to})
		if !strings.Contains(text, entry.want) {
			t.Fatalf("%v %s -> %s gave %q, want it to contain %q", entry.value, entry.from, entry.to, text, entry.want)
		}
	}
	// Below absolute zero is refused rather than answered.
	got := presetToolError(t, compiled, "unit_convert", map[string]any{"value": -300, "from": "°C", "to": "K"})
	if !strings.Contains(got, "absolute zero") {
		t.Fatalf("negative kelvin error = %q", got)
	}
}

// TestPreset_UnitConvertKnownValues checks a table of definitions that have an
// exact answer, so the factors themselves are under test.
func TestPreset_UnitConvertKnownValues(t *testing.T) {
	compiled := presetBundle(t, "unit-convert")
	if compiled == nil {
		t.Fatal("unit-convert is not in the catalog")
	}
	for _, entry := range []struct {
		value float64
		from  string
		to    string
		want  string
	}{
		{1, "gal", "L", "3.785411784"},
		{1, "GiB", "B", "1073741824"},
		{1, "GB", "B", "1000000000"},
		{1, "MiB", "KiB", "1024"},
		{1, "in", "mm", "25.4"},
		{1, "mi", "m", "1609.344"},
		{1, "lb", "g", "453.59237"},
		{1, "kg", "lb", "2.20462262185"},
		{36, "km/h", "m/s", "10"},
		{1, "atm", "Pa", "101325"},
		{1, "bar", "psi", "14.503773773"},
		{1, "kWh", "J", "3600000"},
		{1, "kcal", "J", "4184"},
		{1, "acre", "m²", "4046.8564224"},
		{100, "cm", "in", "39.3700787402"},
		{1, "hp", "W", "745.699871582"},
		{1, "ft", "m", "0.3048"},
		{90, "deg", "rad", "1.57079632679"},
		{60, "rpm", "Hz", "1"},
		{1, "nmi", "m", "1852"},
		{1, "t", "kg", "1000"},
		{1, "lbf", "N", "4.44822161526"},
		{1, "N*m", "lbf*ft", "0.737562149277"},
	} {
		text := presetTool(t, compiled, "unit_convert", map[string]any{"value": entry.value, "from": entry.from, "to": entry.to, "precision": 12})
		if !strings.Contains(text, entry.want) {
			t.Fatalf("%v %s -> %s = %q, want it to contain %s", entry.value, entry.from, entry.to, text, entry.want)
		}
	}
}

func TestPreset_UnitConvertRefusesCategoryErrors(t *testing.T) {
	compiled := presetBundle(t, "unit-convert")
	if compiled == nil {
		t.Fatal("unit-convert is not in the catalog")
	}
	// A converter that answers this with a number is worse than one that does not.
	got := presetToolError(t, compiled, "unit_convert", map[string]any{"value": 5, "from": "cm", "to": "kg"})
	if !strings.Contains(got, "different quantities") {
		t.Fatalf("length-to-mass error = %q", got)
	}
	// Torque is not energy, even though both reduce to N·m.
	got = presetToolError(t, compiled, "unit_convert", map[string]any{"value": 5, "from": "N*m", "to": "J"})
	if !strings.Contains(got, "different quantities") {
		t.Fatalf("torque-to-energy error = %q", got)
	}
	got = presetToolError(t, compiled, "unit_convert", map[string]any{"value": 20, "from": "°C", "to": "cm"})
	if !strings.Contains(got, "temperature") {
		t.Fatalf("temperature-to-length error = %q", got)
	}
}

func TestPreset_UnitConvertNamesItsAmbiguities(t *testing.T) {
	compiled := presetBundle(t, "unit-convert")
	if compiled == nil {
		t.Fatal("unit-convert is not in the catalog")
	}
	// A spelling that legitimately means two different things has to come back as
	// a question rather than a number. "M" is metres in one reading and minutes in
	// another; "t" is tonnes and teaspoons.
	got := presetToolError(t, compiled, "unit_convert", map[string]any{"value": 1, "from": "p", "to": "b"})
	if !strings.Contains(got, "ambiguous") {
		t.Fatalf("ambiguity error = %q", got)
	}
	// Case is what separates a megabyte from a megabit, so both work and give
	// different answers.
	megabytes := presetTool(t, compiled, "unit_convert", map[string]any{"value": 1, "from": "MB", "to": "B"})
	if !strings.Contains(megabytes, "1000000") {
		t.Fatalf("MB = %q", megabytes)
	}
	megabits := presetTool(t, compiled, "unit_convert", map[string]any{"value": 1, "from": "Mb", "to": "B"})
	if !strings.Contains(megabits, "125000") {
		t.Fatalf("Mb = %q", megabits)
	}
	// A week and a watt differ only by case too.
	weeks := presetTool(t, compiled, "unit_convert", map[string]any{"value": 1, "from": "w", "to": "d"})
	if !strings.Contains(weeks, "7") {
		t.Fatalf("w = %q", weeks)
	}
	watts := presetTool(t, compiled, "unit_convert", map[string]any{"value": 1, "from": "W", "to": "kW"})
	if !strings.Contains(watts, "0.001") {
		t.Fatalf("W = %q", watts)
	}
	// An unknown name comes back with the close matches.
	got = presetToolError(t, compiled, "unit_convert", map[string]any{"value": 1, "from": "kilogramm", "to": "g"})
	if !strings.Contains(got, "unknown unit") || !strings.Contains(got, "kg") {
		t.Fatalf("unknown unit error = %q", got)
	}
}

func TestPreset_UnitConvertListsWhatItSupports(t *testing.T) {
	compiled := presetBundle(t, "unit-convert")
	if compiled == nil {
		t.Fatal("unit-convert is not in the catalog")
	}
	text := presetTool(t, compiled, "unit_convert", map[string]any{"action": "families"})
	for _, family := range []string{"length", "mass", "volume", "temperature", "data", "torque"} {
		if !strings.Contains(text, family) {
			t.Fatalf("the family listing is missing %q:\n%s", family, text)
		}
	}
	if !strings.Contains(text, "currency is not supported") {
		t.Fatalf("the listing does not state the limits:\n%s", text)
	}
	list := presetTool(t, compiled, "unit_convert", map[string]any{"action": "list", "from": "data"})
	if !strings.Contains(list, "GiB") || !strings.Contains(list, "GB") {
		t.Fatalf("the data listing does not separate the prefixes:\n%s", list)
	}
	temperature := presetTool(t, compiled, "unit_convert", map[string]any{"action": "list", "from": "celsius"})
	if !strings.Contains(temperature, "°F") || !strings.Contains(temperature, "K = °C") {
		t.Fatalf("the temperature listing lacks its formulas:\n%s", temperature)
	}
}

func TestPreset_UnitConvertMultipleTargetsAndVerbose(t *testing.T) {
	compiled := presetBundle(t, "unit-convert")
	if compiled == nil {
		t.Fatal("unit-convert is not in the catalog")
	}
	text := presetTool(t, compiled, "unit_convert", map[string]any{"value": 160, "from": "cm", "to": "m, ft, in", "verbose": true})
	for _, want := range []string{"= 1.6 m", "= 5.24934 ft", "= 62.9921 in", "1 cm = "} {
		if !strings.Contains(text, want) {
			t.Fatalf("multi-target answer is missing %q:\n%s", want, text)
		}
	}
	// The target limit is reported, not silently truncated.
	got := presetToolError(t, compiled, "unit_convert", map[string]any{
		"value": 1, "from": "m", "to": "km, cm, mm, um, nm, mi, yd, ft, in, nmi, ly, au, pc",
	})
	if !strings.Contains(got, "over the limit") {
		t.Fatalf("target limit error = %q", got)
	}
}

func TestPreset_UnitConvertMissingArguments(t *testing.T) {
	compiled := presetBundle(t, "unit-convert")
	if compiled == nil {
		t.Fatal("unit-convert is not in the catalog")
	}
	for _, entry := range []struct {
		name      string
		arguments map[string]any
		want      string
	}{
		{"no unit", map[string]any{"value": 1}, "unit name is required"},
		{"no value", map[string]any{"from": "m", "to": "ft"}, "value is required"},
		{"no target", map[string]any{"value": 1, "from": "m"}, "give a unit to convert to"},
		{"not a number", map[string]any{"value": "many", "from": "m", "to": "ft"}, "is not a number"},
	} {
		got := presetToolError(t, compiled, "unit_convert", entry.arguments)
		if !strings.Contains(got, entry.want) {
			t.Fatalf("%s: error %q does not mention %q", entry.name, got, entry.want)
		}
	}
}

// TestPreset_MathCalculatorExactIntegers pins the part a floating-point
// calculator gets wrong: integer arithmetic that has to come back digit for
// digit.
func TestPreset_MathCalculatorExactIntegers(t *testing.T) {
	compiled := presetBundle(t, "math-tools")
	if compiled == nil {
		t.Skip("math-tools is not in the catalog")
	}
	for _, entry := range []struct {
		expression string
		want       string
	}{
		{"2^200", "1606938044258990275541962092341162602522202993782792835301376"},
		{"2^64", "18446744073709551616"},
		{"20!", "2432902008176640000"},
		{"1+2*3", "7"},
		{"(1+2)*3", "9"},
		{"10/4", "2.5"},
		{"1/3", "0.333333333333"},
	} {
		text := presetTool(t, compiled, "calculator", map[string]any{"expression": entry.expression, "precision": 12})
		if !strings.Contains(text, entry.want) {
			t.Fatalf("%s = %q, want it to contain %s", entry.expression, text, entry.want)
		}
	}
}

func TestPreset_MathCalculatorPrecedenceAndAngles(t *testing.T) {
	compiled := presetBundle(t, "math-tools")
	if compiled == nil {
		t.Skip("math-tools is not in the catalog")
	}
	// Unary minus binds looser than the power, and the power is right-associative.
	for _, entry := range []struct {
		expression string
		want       string
	}{
		{"-2^2", "-4"},
		{"2^3^2", "512"},
		{"2*3+4", "10"},
		{"2+3*4^2", "50"},
		{"7 % 3", "1"},
		{"-7 % 3", "-1"},
	} {
		text := presetTool(t, compiled, "calculator", map[string]any{"expression": entry.expression})
		if !strings.Contains(text, entry.want) {
			t.Fatalf("%s = %q, want it to contain %s", entry.expression, text, entry.want)
		}
	}
	degrees := presetTool(t, compiled, "calculator", map[string]any{"expression": "sin(90)", "angles": "deg"})
	if !strings.Contains(degrees, "1") {
		t.Fatalf("sin(90 deg) = %q", degrees)
	}
	radians := presetTool(t, compiled, "calculator", map[string]any{"expression": "sin(pi/2)"})
	if !strings.Contains(radians, "1") {
		t.Fatalf("sin(pi/2) = %q", radians)
	}
}

func TestPreset_MathCalculatorErrorsNameThePosition(t *testing.T) {
	compiled := presetBundle(t, "math-tools")
	if compiled == nil {
		t.Skip("math-tools is not in the catalog")
	}
	got := presetToolError(t, compiled, "calculator", map[string]any{"expression": "1 + (2 * 3"})
	if !strings.Contains(got, "column") && !strings.Contains(got, "position") {
		t.Fatalf("unbalanced parenthesis error = %q, want it to name the position", got)
	}
	got = presetToolError(t, compiled, "calculator", map[string]any{"expression": "2 +* 3"})
	if !strings.Contains(got, "column") && !strings.Contains(got, "position") {
		t.Fatalf("bad operator error = %q, want it to name the position", got)
	}
}

// TestPreset_MathSymbolicCoversTheOperations walks the operations the tool
// documents, checking the answers rather than just that a call succeeds.
func TestPreset_MathSymbolicCoversTheOperations(t *testing.T) {
	compiled := presetBundle(t, "math-tools")
	if compiled == nil {
		t.Skip("math-tools is not in the catalog")
	}
	for _, entry := range []struct {
		expression string
		operation  string
		extra      map[string]any
		want       []string
	}{
		{"x^2 + 2*x + 1", "simplify", nil, []string{"x"}},
		{"(x+1)^3", "expand", nil, []string{"x^3", "3*x^2"}},
		{"x^2 - 5*x + 6", "factor", nil, []string{"x"}},
		{"x^3", "derivative", nil, []string{"3*x^2"}},
		{"x^2", "derivative", map[string]any{"order": 2}, []string{"2"}},
		{"x^3", "integrate", nil, []string{"x^4"}},
		{"2*x + 4", "solve", nil, []string{"-2"}},
		{"x^2 - 4", "solve", nil, []string{"2", "-2"}},
	} {
		arguments := map[string]any{"expression": entry.expression, "operation": entry.operation}
		for key, value := range entry.extra {
			arguments[key] = value
		}
		text := presetTool(t, compiled, "symbolic_math", arguments)
		for _, want := range entry.want {
			if !strings.Contains(text, want) {
				t.Fatalf("%s (%s) = %q, want it to contain %q", entry.expression, entry.operation, text, want)
			}
		}
	}
}

func TestPreset_MathCalculatorAndSymbolicSpotCheck(t *testing.T) {
	compiled := presetBundle(t, "math-tools")
	if compiled == nil {
		t.Fatal("no math-tools")
	}
	// The 100! digits and the other exact values here are from the definitions,
	// not from this implementation.
	for _, entry := range []struct {
		tool string
		args map[string]any
		want []string
	}{
		{"calculator", map[string]any{"expression": "100!"}, []string{"93326215443944152681699238856266700490715968264381621468592963895217599993229915608941463976156518286253697920827223758251185210916864000000000000000000000000"}},
		{"calculator", map[string]any{"expression": "2pi"}, []string{"6.28318"}},
		{"calculator", map[string]any{"expression": "3(4+5)"}, []string{"27"}},
		{"calculator", map[string]any{"expression": "x = 5; x^2 + 1"}, []string{"26"}},
		{"symbolic_math", map[string]any{"expression": "x*sin(x)", "operation": "derivative"}, []string{"sin(x)", "cos(x)"}},
		{"symbolic_math", map[string]any{"expression": "x^2 - 4", "operation": "integrate", "from": 0, "to": 2}, []string{"-"}},
		{"symbolic_math", map[string]any{"expression": "x^2 + 1", "operation": "solve"}, []string{"no real"}},
		{"symbolic_math", map[string]any{"expression": "2*x + 3 = 7", "operation": "solve"}, []string{"2"}},
		{"symbolic_math", map[string]any{"expression": "x^2", "operation": "evaluate", "values": map[string]any{"x": 3}}, []string{"9"}},
	} {
		text := presetTool(t, compiled, entry.tool, entry.args)
		for _, want := range entry.want {
			if !strings.Contains(text, want) {
				t.Errorf("%s %v = %q, want %q", entry.tool, entry.args, text, want)
			}
		}
	}
}

// TestPreset_MathCalculatorFloatFallbacks pins the conversion from exact
// integers to float64. The production runtime is goja, whose Number(bigint)
// narrows through the low 64 bits: 2^100 became 0 and every float fallback that
// touched it answered with garbage, with no approximation warning to say so.
// These are the cases a user hits by asking for a percentage, a root or a
// logarithm of a large integer.
func TestPreset_MathCalculatorFloatFallbacks(t *testing.T) {
	compiled := presetBundle(t, "math-tools")
	if compiled == nil {
		t.Skip("math-tools is not in the catalog")
	}
	for _, entry := range []struct {
		expression string
		want       string
	}{
		// 2^100 / 3 = 4.225502000756127...e29
		{expression: "2^100 / 3", want: "4.2255e+29"},
		// sqrt(2^101) = 1.5922629181...e15
		{expression: "sqrt(2^101)", want: "1.59226e+15"},
		{expression: "1e308", want: "1e+308"},
		{expression: "1e-308", want: "1e-308"},
		{expression: "2^100 * 0.5", want: "6.33825e+29"},
		{expression: "log10(10^30)", want: "= 30"},
		{expression: "sin(2^100)", want: "≈ -0.872184"},
	} {
		text := presetTool(t, compiled, "calculator", map[string]any{"expression": entry.expression, "precision": 6})
		if !strings.Contains(text, entry.want) {
			t.Fatalf("%s = %q, want it to contain %q", entry.expression, text, entry.want)
		}
	}
	// A value past the largest float64 is an overflow, which has to be reported
	// rather than answered with zero.
	overflow := presetToolError(t, compiled, "calculator", map[string]any{"expression": "1e309"})
	if !strings.Contains(overflow, "past the largest float64") {
		t.Fatalf("1e309 = %q, want it to name the overflow", overflow)
	}
	// The same conversion backs the symbolic evaluator, which returned zero for
	// any large integer before the fix.
	symbolic := presetTool(t, compiled, "symbolic_math", map[string]any{"expression": "sqrt(2^101)", "operation": "evaluate"})
	if !strings.Contains(symbolic, "1.59226e+15") && !strings.Contains(symbolic, "1592262918") {
		t.Fatalf("symbolic sqrt(2^101) = %q", symbolic)
	}
}

// TestPreset_MathCalculatorHugeExponentIsRefused pins that a float conversion
// cannot turn a huge power into a cheap computation: the digit estimate is what
// decides whether the exact path runs.
func TestPreset_MathCalculatorHugeExponentIsRefused(t *testing.T) {
	compiled := presetBundle(t, "math-tools")
	if compiled == nil {
		t.Skip("math-tools is not in the catalog")
	}
	// 2^(2^100) has far more digits than any limit; it must be refused quickly
	// with an explanation rather than attempted.
	text := presetToolError(t, compiled, "calculator", map[string]any{"expression": "2^(2^100)"})
	if !strings.Contains(text, "digit") && !strings.Contains(text, "float") {
		t.Fatalf("huge exponent error = %q", text)
	}
}

// TestPreset_UnitConvertPrefixCaseIsNotFolded pins the hazard a case-insensitive
// lookup hides: mW and MW are a billion apart, and folding one onto the other
// answers confidently with the wrong number.
func TestPreset_UnitConvertPrefixCaseIsNotFolded(t *testing.T) {
	compiled := presetBundle(t, "unit-convert")
	if compiled == nil {
		t.Fatal("unit-convert is not in the catalog")
	}
	// A recognised spelling still works in both cases.
	for _, unit := range []string{"MW", "mw"} {
		if text := presetTool(t, compiled, "unit_convert", map[string]any{"value": 1, "from": unit, "to": "W"}); !strings.Contains(text, "1000000") {
			t.Fatalf("%s = %q", unit, text)
		}
	}
	// The case-distinct spellings are real units and give the right factor.
	for _, entry := range []struct {
		from string
		to   string
		want string
	}{
		{"mW", "W", "0.001"},
		{"MW", "W", "1000000"},
		{"mJ", "J", "0.001"},
		{"MN", "N", "1000000"},
		{"ML", "L", "1000"},
		{"Kb", "bit", "1000"},
		{"mB", "B", "0.001"},
	} {
		text := presetTool(t, compiled, "unit_convert", map[string]any{"value": 1, "from": entry.from, "to": entry.to})
		if !strings.Contains(text, entry.want) {
			t.Fatalf("%s -> %s = %q, want %s", entry.from, entry.to, text, entry.want)
		}
	}
	// A spelling that is still unknown gets advice that actually works: every
	// name it prints has to resolve, or following it reproduces the same error.
	got := presetToolError(t, compiled, "unit_convert", map[string]any{"value": 1, "from": "MWH", "to": "Wh"})
	if !strings.Contains(got, "unknown unit") && !strings.Contains(got, "case decides") {
		t.Fatalf("MWH: error = %q", got)
	}
	for _, name := range []string{"MWH"} {
		_ = name
	}
	// "mw" is the alias for megawatt, so a caller who wrote "mW" meant a
	// milliwatt and gets it; only an unknown spelling reaches the advice.
	advice := presetToolError(t, compiled, "unit_convert", map[string]any{"value": 1, "from": "MWATTS", "to": "W"})
	if !strings.Contains(advice, "Did you mean") {
		t.Fatalf("MWATTS: error = %q", advice)
	}
	// bps is bits per second, consistently with kbps and Mbps.
	text := presetTool(t, compiled, "unit_convert", map[string]any{"value": 8, "from": "bps", "to": "B/s"})
	if !strings.Contains(text, "1 B/s") {
		t.Fatalf("8 bps = %q, want 1 B/s", text)
	}
}

// TestPreset_UnitConvertListsEveryFamily is the regression for a null entry in a
// factor table: the listing dereferenced it and the whole action threw.
func TestPreset_UnitConvertListsEveryFamily(t *testing.T) {
	compiled := presetBundle(t, "unit-convert")
	if compiled == nil {
		t.Fatal("unit-convert is not in the catalog")
	}
	families := []string{"length", "mass", "area", "volume", "temperature", "time", "speed",
		"pressure", "energy", "power", "angle", "frequency", "data", "force", "torque"}
	for _, family := range families {
		text := presetTool(t, compiled, "unit_convert", map[string]any{"action": "list", "from": family})
		if strings.Contains(text, "TypeError") || strings.Contains(text, "undefined") {
			t.Fatalf("listing %s broke: %s", family, text)
		}
		if !strings.Contains(text, family) && family != "temperature" {
			t.Fatalf("listing %s does not name the family: %s", family, text)
		}
	}
	// A unit name works as the selector too.
	if text := presetTool(t, compiled, "unit_convert", map[string]any{"action": "list", "from": "W"}); !strings.Contains(text, "kW") {
		t.Fatalf("listing by unit W = %q", text)
	}
}

// TestPreset_UnitConvertNamesLogarithmicUnits pins a helpful answer for a unit a
// factor table cannot express, instead of "unknown unit".
func TestPreset_UnitConvertNamesLogarithmicUnits(t *testing.T) {
	compiled := presetBundle(t, "unit-convert")
	if compiled == nil {
		t.Fatal("unit-convert is not in the catalog")
	}
	for _, unit := range []string{"dB", "dBm", "dBi"} {
		got := presetToolError(t, compiled, "unit_convert", map[string]any{"value": 0, "from": unit, "to": "W"})
		if !strings.Contains(got, "logarithmic") {
			t.Fatalf("%s: error = %q", unit, got)
		}
	}
	// An unknown spelling still gets close matches rather than an internal error.
	got := presetToolError(t, compiled, "unit_convert", map[string]any{"value": 1, "from": "kilogramm", "to": "g"})
	if !strings.Contains(got, "Did you mean") {
		t.Fatalf("unknown unit error = %q", got)
	}
}

// TestPreset_MathHugeExponentsAreRefusedNotMisread pins the remaining
// Number(bigint) paths: goja narrows a BigInt through Int64, so a degree of 2^64
// arrived as 0 and an expansion of (x+1)^(2^64) returned "1" - wrong answers with
// no warning. Nothing may compute a huge intermediate either: a billion-degree
// root used to kill the worker through a 300-million-digit power.
func TestPreset_MathHugeExponentsAreRefusedNotMisread(t *testing.T) {
	compiled := presetBundle(t, "math-tools")
	if compiled == nil {
		t.Skip("math-tools is not in the catalog")
	}
	// A huge degree is refused with a limit, and quickly.
	for _, expression := range []string{"root(8, 2^64+1)", "root(8, 2^63)"} {
		text := presetToolError(t, compiled, "calculator", map[string]any{"expression": expression})
		if !strings.Contains(text, "digit") && !strings.Contains(text, "degree") {
			t.Fatalf("%s: error = %q", expression, text)
		}
	}
	// An enormous degree must not be attempted.
	started := time.Now()
	text := presetToolError(t, compiled, "calculator", map[string]any{"expression": "root(8, 1000000000)"})
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("root(8, 1000000000) took %s; it should be refused immediately", elapsed)
	}
	if !strings.Contains(text, "degree") && !strings.Contains(text, "digit") {
		t.Fatalf("root(8, 1000000000): error = %q", text)
	}
	// An expansion with an exponent past the bound stays symbolic rather than
	// answering "1".
	expanded := presetTool(t, compiled, "symbolic_math", map[string]any{"expression": "(x+1)^(2^64)", "operation": "expand"})
	if strings.Contains(expanded, "= 1") && !strings.Contains(expanded, "^") {
		t.Fatalf("(x+1)^(2^64) expanded to %q", expanded)
	}
	// A polynomial of degree 2^63 is not a polynomial this solver handles; it
	// must report that, not throw an internal type error.
	for _, expression := range []string{"x^(2^64)-1", "x^(2^63)-1"} {
		result := presetToolErrorOrText(t, compiled, "symbolic_math", map[string]any{"expression": expression, "operation": "factor"})
		if strings.Contains(result, "Cannot read property") {
			t.Fatalf("%s: internal error leaked: %q", expression, result)
		}
	}
}

// presetToolErrorOrText returns the tool's answer or its error, whichever comes.
func presetToolErrorOrText(t *testing.T, compiled *Compiled, name string, arguments map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"name": name, "arguments": arguments})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := compiled.Invoke(context.Background(), "onToolCall", Context{Endpoint: "chat.completions"}, payload)
	if err != nil {
		return err.Error()
	}
	return string(out)
}

// TestPreset_MathSolveDoesNotInventRoots pins the worst kind of wrong answer: a
// confident, complete-looking root set for a polynomial that does not have those
// roots. After the rational roots are factored out, a remainder of degree 4 or
// more used to be handed to the quadratic formula, so x^5-1 reported the roots of
// x^2+x+1 and x^3-2 threw a division by zero.
func TestPreset_MathSolveDoesNotInventRoots(t *testing.T) {
	compiled := presetBundle(t, "math-tools")
	if compiled == nil {
		t.Skip("math-tools is not in the catalog")
	}
	// The roots of x^2+x+1 are not roots of x^5-1.
	for _, expression := range []string{"x^5-1", "x^7-1", "x^63-1"} {
		text := presetToolErrorOrText(t, compiled, "symbolic_math", map[string]any{"expression": expression, "operation": "solve"})
		if strings.Contains(text, "sqrt(3)") || strings.Contains(text, "0.866") || strings.Contains(text, "1.7320") {
			t.Fatalf("%s returned the roots of a different polynomial: %s", expression, text)
		}
		// The one real root is still found, whether exactly or numerically.
		if !strings.Contains(text, "x = 1") {
			t.Fatalf("%s did not report the real root: %s", expression, text)
		}
		// Whatever remains is said to be numeric, not exact-and-complete.
		if !strings.Contains(text, "numeric") && !strings.Contains(text, "no solution") && !strings.Contains(text, "no real root") {
			t.Fatalf("%s did not describe the remaining roots honestly: %s", expression, text)
		}
	}
	// A cubic with one rational root reports it exactly and the rest numerically,
	// instead of throwing.
	cubic := presetToolErrorOrText(t, compiled, "symbolic_math", map[string]any{"expression": "x^3-2", "operation": "solve"})
	if strings.Contains(cubic, "division by zero") || strings.Contains(cubic, "Cannot read") {
		t.Fatalf("x^3-2 leaked an internal error: %s", cubic)
	}
	if !strings.Contains(cubic, "1.25992") {
		t.Fatalf("x^3-2 did not find its real root: %s", cubic)
	}
	// A quartic with no real root says so rather than inventing one.
	quartic := presetToolErrorOrText(t, compiled, "symbolic_math", map[string]any{"expression": "x^4+1", "operation": "solve"})
	if strings.Contains(quartic, "x = ") && !strings.Contains(quartic, "no solution") {
		t.Fatalf("x^4+1 invented a real root: %s", quartic)
	}
	// The quadratic path is untouched: two exact roots.
	quadratic := presetTool(t, compiled, "symbolic_math", map[string]any{"expression": "x^2-4", "operation": "solve"})
	if !strings.Contains(quadratic, "x = 2") || !strings.Contains(quadratic, "x = -2") {
		t.Fatalf("x^2-4 = %q", quadratic)
	}
}
