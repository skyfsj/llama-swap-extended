package config

import (
	"bytes"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func pruneForTest(t *testing.T, before, after string) (string, []string) {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(after), &doc); err != nil {
		t.Fatalf("unmarshal patched document: %v", err)
	}
	var original yaml.Node
	if err := yaml.Unmarshal([]byte(before), &original); err != nil {
		t.Fatalf("unmarshal original document: %v", err)
	}
	pruned := PruneRemovedModels(&doc, RemovedModelKeys(ModelKeys(&original), &doc))
	out, err := yaml.Marshal(&doc)
	if err != nil {
		t.Fatalf("marshal pruned document: %v", err)
	}
	return string(out), pruned
}

// dropModelBlock removes one model's block from a config document written with
// the two-space indentation the tests use: the entry line plus every line
// indented deeper than it, up to the next model or a shallower key.
func dropModelBlock(document, id string) string {
	kept := make([]string, 0, len(document)/32)
	inBlock := false
	for _, line := range strings.Split(document, "\n") {
		if line == "  "+id+":" {
			inBlock = true
			continue
		}
		if inBlock {
			if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "    ") {
				continue
			}
			inBlock = false
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// TestPruneRemovedModels_Matrix covers the reported failure: deleting a model
// the matrix references is rejected by the loader, because vars, evict_costs
// and the set expressions all validate against the live model set.
func TestPruneRemovedModels_Matrix(t *testing.T) {
	before := `
models:
  a:
    cmd: echo ${PORT}
  b:
    cmd: echo ${PORT}
  c:
    cmd: echo ${PORT}
routing:
  router:
    use: matrix
    settings:
      matrix:
        vars:
          small: a
          mid: b
        evict_costs:
          a: 5
          mid: 2
        sets:
          pair: "small & mid"
          triple: "small & mid & c"
          alternative: "small | b"
          nested: "(small | mid) & c"
          referenced: "small & +pair"
`
	// Model 'a' is gone; the surviving document still names it everywhere.
	after := dropModelBlock(before, "a")

	pruned, locations := pruneForTest(t, before, after)
	t.Logf("pruned locations: %v", locations)

	if strings.Contains(pruned, "small") {
		t.Errorf("pruned matrix still references the removed model through var 'small':\n%s", pruned)
	}
	if _, err := LoadConfigFromReader(bytes.NewReader([]byte(pruned))); err != nil {
		t.Fatalf("pruned document does not load: %v\n%s", err, pruned)
	}
	if strings.Contains(pruned, "          a: 5") {
		t.Errorf("evict_costs still lists the removed model:\n%s", pruned)
	}
	if strings.Contains(pruned, "small & ") || strings.Contains(pruned, "& small") {
		t.Errorf("set expression still ANDs the removed model:\n%s", pruned)
	}
	// A single surviving child renders bare, so a collapsed expression keeps the
	// surviving model without an operator or parentheses.
	for set, want := range map[string]string{
		"pair":        "mid",
		"triple":      "mid & c",
		"alternative": "b",
		"nested":      "mid & c",
	} {
		if !strings.Contains(pruned, set+`: "`+want+`"`) {
			t.Errorf("set %q should render as %q:\n%s", set, want, pruned)
		}
	}
	if !strings.Contains(pruned, `referenced: "+pair"`) {
		t.Errorf("the reference to the surviving set should be kept:\n%s", pruned)
	}
}

// TestPruneRemovedModels_MatrixDropsEmptySets proves a set whose whole
// expression named removed models is deleted, and that sets which referenced
// it are adjusted in the next pass.
func TestPruneRemovedModels_MatrixDropsEmptySets(t *testing.T) {
	before := `
models:
  a:
    cmd: echo ${PORT}
  b:
    cmd: echo ${PORT}
routing:
  router:
    use: matrix
    settings:
      matrix:
        sets:
          only-a: "a"
          uses-a: "+only-a & b"
          keeps-b: "b"
`
	after := dropModelBlock(before, "a")

	pruned, _ := pruneForTest(t, before, after)
	if strings.Contains(pruned, "only-a") {
		t.Errorf("emptied set 'only-a' should be deleted:\n%s", pruned)
	}
	// The set that referenced it survives with the only model left in it.
	if !strings.Contains(pruned, `uses-a: "b"`) {
		t.Errorf("the referencing set should survive with model b only:\n%s", pruned)
	}
	if !strings.Contains(pruned, `keeps-b: "b"`) {
		t.Errorf("the untouched set should survive verbatim:\n%s", pruned)
	}
	if _, err := LoadConfigFromReader(bytes.NewReader([]byte(pruned))); err != nil {
		t.Fatalf("pruned document does not load: %v\n%s", err, pruned)
	}
}

// TestPruneRemovedModels_LastMatrixModelFallsBackToGroup proves the cascade
// completes: removing the last model in the matrix removes the matrix and the
// router selection, so the loader synthesizes the default group instead of
// rejecting the delete.
func TestPruneRemovedModels_LastMatrixModelFallsBackToGroup(t *testing.T) {
	before := `
models:
  a:
    cmd: echo ${PORT}
  b:
    cmd: echo ${PORT}
routing:
  router:
    use: matrix
    settings:
      matrix:
        sets:
          only: "a"
`
	after := dropModelBlock(before, "a")

	pruned, _ := pruneForTest(t, before, after)
	cfg, err := LoadConfigFromReader(bytes.NewReader([]byte(pruned)))
	if err != nil {
		t.Fatalf("pruned document does not load: %v\n%s", err, pruned)
	}
	if cfg.Matrix != nil {
		t.Errorf("matrix should be gone, got %+v", cfg.Matrix)
	}
	if cfg.Routing.Router.Use != "group" {
		t.Errorf("router = %q, want the default group engine", cfg.Routing.Router.Use)
	}
	if len(cfg.Groups) == 0 {
		t.Error("expected a default group so the surviving model stays routable")
	}
}

// TestPruneRemovedModels_OtherRoutingBlocks covers the other reference sites
// that reject a delete for the same reason: group members, selector targets,
// gpus card lists, the scheduler priority map and profile pins.
func TestPruneRemovedModels_OtherRoutingBlocks(t *testing.T) {
	before := `
models:
  a:
    cmd: echo ${PORT}
  b:
    cmd: echo ${PORT}
routing:
  router:
    settings:
      groups:
        first:
          swap: false
          members: [a, b]
        second:
          swap: false
          members: [a]
  scheduler:
    settings:
      fifo:
        priority:
          a: 10
          b: 1
selectors:
  coding:
    strategy: warm
    targets: [a, b]
  solo:
    strategy: pin
    targets: [a]
profiles:
  fast:
    pins:
      a: b
      slow: a
`
	after := dropModelBlock(before, "a")

	pruned, _ := pruneForTest(t, before, after)
	t.Logf("pruned:\n%s", pruned)
	cfg, err := LoadConfigFromReader(bytes.NewReader([]byte(pruned)))
	if err != nil {
		t.Fatalf("pruned document does not load: %v\n%s", err, pruned)
	}
	for _, group := range cfg.Groups {
		for _, member := range group.Members {
			if member == "a" {
				t.Errorf("group %+v still lists the removed model", group)
			}
		}
	}
	if _, found := cfg.Groups["second"]; found {
		t.Error("a group left with no member should be removed")
	}
	if _, found := cfg.Groups["first"]; !found {
		t.Error("a group with a surviving member should stay")
	}
	if priority := cfg.Routing.Scheduler.Settings.Fifo.Priority; priority != nil {
		if _, found := priority["a"]; found {
			t.Error("fifo priority still lists the removed model")
		}
	}
	if _, found := cfg.Selectors["solo"]; found {
		t.Error("a selector left with no target should be removed")
	}
	if _, found := cfg.Selectors["coding"]; !found {
		t.Error("a selector with a surviving target should stay")
	}
	for _, profile := range cfg.Profiles {
		for pin, target := range profile.Pins {
			if target == "a" {
				t.Errorf("profile pin %q still targets the removed model", pin)
			}
		}
	}
}

// TestPruneRemovedModels_GpusCards covers the gpus card lists, where an empty
// card and a removed router selection would each fail the loader.
func TestPruneRemovedModels_GpusCards(t *testing.T) {
	before := `
models:
  a:
    cmd: echo ${PORT}
  b:
    cmd: echo ${PORT}
routing:
  router:
    use: gpus
    settings:
      gpus:
        "0": [a, b]
        "1": [a]
`
	after := dropModelBlock(before, "a")

	pruned, _ := pruneForTest(t, before, after)
	cfg, err := LoadConfigFromReader(bytes.NewReader([]byte(pruned)))
	if err != nil {
		t.Fatalf("pruned document does not load: %v\n%s", err, pruned)
	}
	cards := cfg.Routing.Router.Settings.Gpus
	if cards == nil {
		t.Fatalf("gpus config disappeared, want only the emptied card removed:\n%s", pruned)
	}
	if _, found := cards.Cards["1"]; found {
		t.Error("a card left with no model should be removed")
	}
	if models := cards.Cards["0"]; len(models) != 1 || models[0] != "b" {
		t.Errorf("card 0 = %v, want [b]", models)
	}
	if cfg.Routing.Router.Use != "gpus" {
		t.Errorf("router = %q, want gpus kept", cfg.Routing.Router.Use)
	}
}

// TestPruneRemovedModels_TouchesNothingElse proves the pruning is limited to
// the removed model's references: a config that never named it is returned
// with no reported changes, and models added by the same patch survive.
func TestPruneRemovedModels_TouchesNothingElse(t *testing.T) {
	before := `
models:
  a:
    cmd: echo ${PORT}
  b:
    cmd: echo ${PORT}
routing:
  router:
    settings:
      groups:
        first:
          swap: false
          members: [b]
`
	// Nothing was removed, so nothing may be pruned. The document is compared
	// through the same marshal round trip, which re-indents it.
	roundTripped := func(document string) string {
		var node yaml.Node
		if err := yaml.Unmarshal([]byte(document), &node); err != nil {
			t.Fatalf("unmarshal %q: %v", document, err)
		}
		out, err := yaml.Marshal(&node)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(out)
	}
	pruned, locations := pruneForTest(t, before, before)
	if len(locations) != 0 {
		t.Errorf("reported changes for an untouched config: %v", locations)
	}
	if pruned != roundTripped(before) {
		t.Errorf("document changed without a removal:\n%s", pruned)
	}

	// A model removed and re-added by the same patch is not gone.
	after := strings.Replace(before, `  b:
    cmd: echo ${PORT}
`, `  b:
    cmd: echo ${PORT}
  c:
    cmd: echo ${PORT}
`, 1)
	pruned, locations = pruneForTest(t, before, after)
	if len(locations) != 0 {
		t.Errorf("reported changes for an addition: %v", locations)
	}
	if !strings.Contains(pruned, "c:") {
		t.Errorf("added model disappeared:\n%s", pruned)
	}
}
